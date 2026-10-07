//go:build e2e

package e2e

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// makeDB creates a SQLite database at path holding rows rows in table m.
func makeDB(t *testing.T, e *env, sqlite, path string, rows int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	e.must(filepath.Dir(path), sqlite, path, "CREATE TABLE m(x); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<"+strconv.Itoa(rows)+") INSERT INTO m SELECT i FROM n;")
}

// restoredFiles restores the backup and lists its files.
func restoredFiles(t *testing.T, e *env, b *backupRepo) (string, []string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored")
	e.must(b.base, "salt", "restore", b.dir, "--to", dest)
	var files []string
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dest, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	})
	slices.Sort(files)
	return dest, files
}

// assertDB checks the database at p opens, passes integrity_check and holds
// rows rows.
func assertDB(t *testing.T, e *env, sqlite, p string, rows int) {
	t.Helper()
	out := e.must(filepath.Dir(p), sqlite, p, "SELECT count(*) FROM m; PRAGMA integrity_check;")
	if f := strings.Fields(out); len(f) != 2 || f[0] != strconv.Itoa(rows) || f[1] != "ok" {
		t.Fatalf("%s holds:\n%s", p, out)
	}
}

// salt backup --preset mnemosyne backs up Mnemosyne's memory in Hermes and a
// Hermes profile, and on its own, while the main database is in use with its
// newest rows only in the -wal file. It leaves out a settings file holding
// an API key, models, logs and the .env file, commits and pushes. The
// restored databases open and hold every row. A second run with nothing
// changed makes no commit.
func TestBackupMnemosyne(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	hermes := filepath.Join(e.home, ".hermes")
	os.MkdirAll(filepath.Join(hermes, "mnemosyne", "data"), 0o755)
	startAgent(t, e, sqlite, filepath.Join(hermes, "mnemosyne", "data", "mnemosyne.db"), 100)
	makeDB(t, e, sqlite, filepath.Join(hermes, "mnemosyne", "data", "banks", "work", "mnemosyne.db"), 7)
	makeDB(t, e, sqlite, filepath.Join(hermes, "profiles", "coder", "mnemosyne", "data", "mnemosyne.db"), 3)
	makeDB(t, e, sqlite, filepath.Join(e.home, ".mnemosyne", "data", "shared", "mnemosyne.db"), 2)
	write(t, filepath.Join(hermes, "mnemosyne", "config.yaml"), "vec_weight: 0.5\nllm_api_key: \"\"\n")
	write(t, filepath.Join(hermes, "profiles", "coder", "mnemosyne", "config.yaml"), "llm_api_key: sk-secret\n")
	write(t, filepath.Join(hermes, "profiles", "writer", "mnemosyne", "config.yaml"), "providers:\n  openai:\n    apiKey: sk-secret\n")
	write(t, filepath.Join(hermes, "mnemosyne", "blobs", "ab", "abcd", "abcdef"), "an attached image")
	write(t, filepath.Join(hermes, "mnemosyne", "models", "model.gguf"), "a downloaded model")
	write(t, filepath.Join(hermes, "mnemosyne", "logs", "diagnose.log"), "a log")
	write(t, filepath.Join(hermes, ".env"), "OPENAI_API_KEY=sk-secret")
	write(t, filepath.Join(e.home, ".mnemosyne", ".env"), "MNEMOSYNE_LLM_API_KEY=sk-secret")
	commits := commitCount(e, b.remote)

	out := e.must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	want := "salt: left ~/.hermes/profiles/coder/mnemosyne/config.yaml out of the backup because its setting llm_api_key holds a secret. Keep secrets in environment variables so the file can be backed up\n" +
		"salt: left ~/.hermes/profiles/writer/mnemosyne/config.yaml out of the backup because its setting apiKey holds a secret. Keep secrets in environment variables so the file can be backed up\n"
	if out != want {
		t.Fatalf("backup printed:\n%s", out)
	}
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	if subject := strings.TrimSpace(e.must(b.remote, "git", "log", "-1", "--format=%s")); subject != "salt backup" {
		t.Fatalf("pushed commit %q", subject)
	}
	dest, files := restoredFiles(t, e, b)
	wantFiles := []string{
		"hermes/mnemosyne/config.yaml",
		"hermes/mnemosyne/data/banks/work/mnemosyne.db",
		"hermes/mnemosyne/data/mnemosyne.db",
		"hermes/profiles/coder/mnemosyne/data/mnemosyne.db",
		"mnemosyne-blobs/ab/abcd/abcdef",
		"mnemosyne-home/data/shared/mnemosyne.db",
	}
	if !slices.Equal(files, wantFiles) {
		t.Fatalf("restored %v, want %v", files, wantFiles)
	}
	assertDB(t, e, sqlite, filepath.Join(dest, "hermes/mnemosyne/data/mnemosyne.db"), 100)
	assertDB(t, e, sqlite, filepath.Join(dest, "hermes/mnemosyne/data/banks/work/mnemosyne.db"), 7)
	assertDB(t, e, sqlite, filepath.Join(dest, "hermes/profiles/coder/mnemosyne/data/mnemosyne.db"), 3)
	assertDB(t, e, sqlite, filepath.Join(dest, "mnemosyne-home/data/shared/mnemosyne.db"), 2)
	// The live database is left in use as it was.
	if _, err := os.Stat(filepath.Join(hermes, "mnemosyne", "data", "mnemosyne.db-wal")); err != nil {
		t.Fatalf("the live -wal file: %v", err)
	}

	commits = commitCount(e, b.remote)
	e.must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	// MNEMOSYNE_DATA_DIR names a data folder outside Hermes.
	data := filepath.Join(t.TempDir(), "memory")
	makeDB(t, e, sqlite, filepath.Join(data, "mnemosyne.db"), 4)
	e.with("MNEMOSYNE_DATA_DIR="+data).must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	dest, files = restoredFiles(t, e, b)
	if !slices.Contains(files, "mnemosyne-data/mnemosyne.db") {
		t.Fatalf("restored %v", files)
	}
	assertDB(t, e, sqlite, filepath.Join(dest, "mnemosyne-data/mnemosyne.db"), 4)
}

// salt backup --preset hermes backs up Hermes's memory, persona, settings,
// skills and databases, in Hermes and each profile, while state.db is in use
// with its newest rows only in the -wal file. It leaves out credentials,
// logs, sessions, caches, downloaded models, the source checkout, what is
// not on the preset's list, and a profile's settings file holding an API
// key, but backs up those that only name the variable a key is kept in,
// as ${NAME} or ${env:NAME}.
// The restored files are the same and the restored databases open and hold
// every row. A second run with nothing changed makes no commit. With
// --preset mnemosyne too, both are backed up together, and HERMES_HOME
// names another Hermes folder.
func TestBackupHermes(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	hermes := filepath.Join(e.home, ".hermes")
	os.MkdirAll(hermes, 0o755)
	startAgent(t, e, sqlite, filepath.Join(hermes, "state.db"), 100)
	dbs := map[string]int{
		"hermes/kanban.db":                                  5,
		"hermes/kanban/boards/website/kanban.db":            4,
		"hermes/projects.db":                                3,
		"hermes/cron/notepad.db":                            2,
		"hermes/profiles/coder/state.db":                    6,
		"hermes/profiles/coder/kanban/boards/ops/kanban.db": 1,
	}
	for rel, rows := range dbs {
		makeDB(t, e, sqlite, filepath.Join(e.home, "."+rel), rows)
	}
	dbs["hermes/state.db"] = 100
	// Each file backed up, by its path in the backup, which is its path
	// under the home folder without the leading dot.
	kept := map[string]string{
		"hermes/config.yaml":                                          "model:\n  default: hermes-4\n  api_key: ${MODEL_API_KEY}\n",
		"hermes/SOUL.md":                                              "You are Hermes.",
		"hermes/profile.yaml":                                         "description: the main agent\n",
		"hermes/channel_aliases.json":                                 `{"telegram": {"-100": "family"}}`,
		"hermes/pairing/discord-approved.json":                        `{"2": {}}`,
		"hermes/platforms/pairing/telegram-approved.json":             `{"1": {}}`,
		"hermes/memories/MEMORY.md":                                   "The user deploys on Fridays.",
		"hermes/memories/USER.md":                                     "Prefers short answers.",
		"hermes/cron/jobs.json":                                       `{"jobs": []}`,
		"hermes/cron/output/daily/run.md":                             "a job's output",
		"hermes/skills/notes/SKILL.md":                                "a skill",
		"hermes/skills/.archive/old/SKILL.md":                         "an archived skill",
		"hermes/reference/api.md":                                     "a reference",
		"hermes/skins/dark.yaml":                                      "colour: black\n",
		"hermes/plans/launch.md":                                      "a plan",
		"hermes/profiles/coder/SOUL.md":                               "You write code.",
		"hermes/profiles/coder/profile.yaml":                          "description: writes code\n",
		"hermes/profiles/coder/channel_aliases.json":                  `{"slack": {"C1": "builds"}}`,
		"hermes/profiles/coder/platforms/pairing/slack-approved.json": `{"3": {}}`,
		"hermes/profiles/coder/memories/MEMORY.md":                    "The repo uses Go.",
		"hermes/profiles/coder/skills/go/SKILL.md":                    "a Go skill",
		"hermes/profiles/writer/config.yaml":                          "display:\n  skin: dark\nmodel:\n  api_key: ${env:WRITER_KEY}\n",
		"hermes/profiles/writer/memories/USER.md":                     "Writes in British English.",
		"hermes/profiles/writer/cron/output/r.md":                     "a profile job's output",
		"hermes/profiles/writer/plans/novel/ch1.md":                   "a profile's plan",
	}
	for rel, content := range kept {
		write(t, filepath.Join(e.home, "."+rel), content)
	}
	for _, rel := range []string{
		".env", "auth.json", "vault.key", "vault.json.enc", "gateway.pid", "memory_store.db",
		"logs/agent.log", "sessions/s1.json", "cache/images/x.png", "browser-profile/Cookies",
		"hermes-agent/README.md", "models/model.gguf", "state-snapshots/s/state.db", "backups/b.zip",
		"kanban/boards/website/workspaces/t1/notes.md", "kanban/boards/website/logs/w.log",
		"channel_directory.json", "profiles/coder/channel_directory.json", "cron/executions.db", "response_store.db", "verification_evidence.db",
		"skills/notes/.env", "skills/notes/prod.env", "skills/notes/auth.json", "skills/notes/vault.key", "skills/notes/vault.json.enc", "skills/notes/__pycache__/x.pyc",
		"skills/notes/node_modules/x/index.js", "skills/notes/.venv/lib/x.py", "skills/notes/.cache/x",
		"profiles/coder/.env", "profiles/coder/auth.json", "profiles/coder/logs/x.log",
		"profiles/.deleted/old/SOUL.md",
	} {
		write(t, filepath.Join(hermes, rel), "left out")
	}
	write(t, filepath.Join(hermes, "profiles", "coder", "config.yaml"), "model:\n  api_key: sk-secret\n")
	commits := commitCount(e, b.remote)

	out := e.must(b.base, "salt", "backup", "--preset", "hermes", b.dir)
	leftOut := "salt: left ~/.hermes/profiles/coder/config.yaml out of the backup because its setting api_key holds a secret. Keep secrets in environment variables so the file can be backed up\n"
	if out != leftOut {
		t.Fatalf("backup printed:\n%s", out)
	}
	if strings.Contains(out, "sk-secret") {
		t.Fatal("the backup showed a secret")
	}
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	wantFiles := slices.Sorted(slices.Values(slices.Concat(slices.Collect(maps.Keys(kept)), slices.Collect(maps.Keys(dbs)))))
	if !slices.Equal(files, wantFiles) {
		t.Fatalf("restored %v, want %v", files, wantFiles)
	}
	for rel, content := range kept {
		if got, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(got) != content {
			t.Errorf("restored %s holds %q, %v", rel, got, err)
		}
	}
	for rel, rows := range dbs {
		assertDB(t, e, sqlite, filepath.Join(dest, rel), rows)
	}
	// The live database is left in use as it was.
	if _, err := os.Stat(filepath.Join(hermes, "state.db-wal")); err != nil {
		t.Fatalf("the live -wal file: %v", err)
	}

	commits = commitCount(e, b.remote)
	e.must(b.base, "salt", "backup", "--preset", "hermes", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	// Mnemosyne's memory inside Hermes is backed up beside it.
	makeDB(t, e, sqlite, filepath.Join(hermes, "mnemosyne", "data", "mnemosyne.db"), 9)
	if out := e.must(b.base, "salt", "backup", "--preset", "hermes", "--preset", "mnemosyne", b.dir); out != leftOut {
		t.Fatalf("backup with mnemosyne printed:\n%s", out)
	}
	dest, files = restoredFiles(t, e, b)
	if !slices.Equal(files, slices.Sorted(slices.Values(slices.Concat(wantFiles, []string{"hermes/mnemosyne/data/mnemosyne.db"})))) {
		t.Fatalf("restored with mnemosyne %v", files)
	}
	assertDB(t, e, sqlite, filepath.Join(dest, "hermes/mnemosyne/data/mnemosyne.db"), 9)

	// HERMES_HOME names the Hermes folder in place of ~/.hermes.
	other := filepath.Join(t.TempDir(), "hermes")
	write(t, filepath.Join(other, "SOUL.md"), "another Hermes")
	e.with("HERMES_HOME="+other).must(b.base, "salt", "backup", "--preset", "hermes", b.dir)
	if _, files = restoredFiles(t, e, b); !slices.Equal(files, []string{"hermes/SOUL.md"}) {
		t.Fatalf("restored with HERMES_HOME %v", files)
	}
}

// salt backup --preset hermes stops, and says where it looked, when it finds
// nothing, including when the only files there hold credentials, and when
// HERMES_HOME names an empty folder. Nothing is committed.
func TestBackupHermesFindsNothing(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	before := commitCount(e, b.dir)
	hermes := filepath.Join(e.home, ".hermes")
	for i, setUp := range []func(){
		func() {},
		func() {
			for _, rel := range []string{".env", "auth.json", "logs/agent.log", "profiles/coder/.env"} {
				write(t, filepath.Join(hermes, rel), "API_KEY=sk-secret")
			}
		},
	} {
		setUp()
		out, code := e.run(b.base, "salt", "backup", "--preset", "hermes", b.dir)
		if code != 1 || !strings.Contains(out, "found nothing to back up for the hermes preset. It looks in ~/.hermes/config.yaml, ~/.hermes/SOUL.md,") ||
			!strings.Contains(out, "~/.hermes/profiles/*/plans") || strings.Contains(out, "sk-secret") {
			t.Fatalf("case %d: exit %d:\n%s", i, code, out)
		}
	}
	empty := t.TempDir()
	out, code := e.with("HERMES_HOME="+empty).run(b.base, "salt", "backup", "--preset", "hermes", b.dir)
	if code != 1 || !strings.Contains(out, "It looks in "+filepath.Join(empty, "config.yaml")+",") || strings.Contains(out, "~/.hermes") {
		t.Fatalf("empty HERMES_HOME: exit %d:\n%s", code, out)
	}
	if got := commitCount(e, b.dir); got != before {
		t.Fatal("a backup that found nothing was committed")
	}
}

// salt backup --preset holographic backs up the Holographic memory
// provider's database while it is in use, with its newest rows only in the
// -wal file. The restored database opens and holds every row, and a second
// run with nothing changed makes no commit. A Hermes folder without the
// database stops the backup with nothing pushed. Which files the preset
// finds is tested in internal/preset.
func TestBackupHolographic(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	hermes := filepath.Join(e.home, ".hermes")
	if err := os.MkdirAll(hermes, 0o755); err != nil {
		t.Fatal(err)
	}
	startAgent(t, e, sqlite, filepath.Join(hermes, "memory_store.db"), 100)
	commits := commitCount(e, b.remote)

	if out := e.must(b.base, "salt", "backup", "--preset", "holographic", b.dir); out != "" {
		t.Fatalf("backup printed:\n%s", out)
	}
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	if !slices.Equal(files, []string{"hermes/memory_store.db"}) {
		t.Fatalf("restored %v", files)
	}
	assertDB(t, e, sqlite, filepath.Join(dest, "hermes/memory_store.db"), 100)
	// The live database is left in use as it was.
	if _, err := os.Stat(filepath.Join(hermes, "memory_store.db-wal")); err != nil {
		t.Fatalf("the live -wal file: %v", err)
	}
	commits = commitCount(e, b.remote)
	e.must(b.base, "salt", "backup", "--preset", "holographic", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	empty := t.TempDir()
	write(t, filepath.Join(empty, "SOUL.md"), "a Hermes without Holographic memory")
	out, code := e.with("HERMES_HOME="+empty).run(b.base, "salt", "backup", "--preset", "hermes", "--preset", "holographic", b.dir)
	if code != 1 || !strings.Contains(out, "found nothing to back up for the holographic preset") {
		t.Fatalf("no database: exit %d:\n%s", code, out)
	}
	if got := commitCount(e, b.remote); got != commits {
		t.Fatal("a backup that found nothing was pushed")
	}
}

// salt backup stops, and says why, when the preset finds nothing, when
// sqlite3 makes no copy of a database, and when the push fails, without
// showing a password in origin's URL.
func TestBackupFailures(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	out, code := e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "found nothing to back up for the mnemosyne preset. It looks in ~/.hermes/mnemosyne/data,") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	before := commitCount(e, b.dir)

	// A copy sqlite3 did not make is not taken for a database that went away
	// while salt backed it up, which salt backup leaves out.
	db := filepath.Join(e.home, ".hermes", "mnemosyne", "data", "mnemosyne.db")
	write(t, db, "SQLite format 3\x00pages")
	out, code = e.with(e.stubPath(t, "sqlite3", "exit 0")).run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "/.hermes/mnemosyne/data/mnemosyne.db: sqlite3 finished without making a copy") {
		t.Fatalf("no copy made: exit %d:\n%s", code, out)
	}
	if got := commitCount(e, b.dir); got != before {
		t.Fatal("a backup without the database was committed")
	}
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "x"), "a file")
	e.must(b.dir, "git", "remote", "set-url", "origin", "https://agent:hunter2@127.0.0.1:1/backup.git")
	out, code = e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "salt could not check origin, so old backups were not dropped and it was not pushed: git ls-remote") || strings.Contains(out, "hunter2") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := commitCount(e, b.dir); got == before {
		t.Fatal("the backup was not committed")
	}

	// A backup pushed from elsewhere is never overwritten.
	e.must(b.dir, "git", "remote", "set-url", "origin", b.remote)
	other := filepath.Join(b.base, "other")
	e.must(b.base, "git", "clone", "-q", b.remote, other)
	e.must(other, "git", "commit", "-q", "--allow-empty", "-m", "elsewhere")
	e.must(other, "git", "push", "-q")
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "y"), "another file")
	out, code = e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "not pushed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if subject := strings.TrimSpace(e.must(b.remote, "git", "log", "-1", "--format=%s")); subject != "elsewhere" {
		t.Fatalf("the remote's latest commit is %q", subject)
	}
	// Fetching it, as an editor might in the background, does not let salt
	// overwrite it either.
	e.must(b.dir, "git", "fetch", "-q", "origin")
	out, code = e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "salt will not overwrite it") {
		t.Fatalf("after a fetch: exit %d:\n%s", code, out)
	}
	if subject := strings.TrimSpace(e.must(b.remote, "git", "log", "-1", "--format=%s")); subject != "elsewhere" {
		t.Fatalf("after a fetch, the remote's latest commit is %q", subject)
	}
}

// A commit pushed by hand from this machine is already in the local branch,
// so salt backup pushes on top of it, even when its prune then rewrites
// the branch so that it no longer holds that commit.
func TestBackupAfterAPushByHand(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "x"), "a file")
	e.must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	write(t, filepath.Join(b.dir, "README.md"), "my backups")
	e.must(b.dir, "git", "add", "README.md")
	e.must(b.dir, "git", "commit", "-qm", "readme")
	e.must(b.dir, "git", "push", "-q", "origin", "HEAD")
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "y"), "another file")
	// One day is kept, so the set-up commit, from an earlier day, is dropped.
	e.must(b.base, "salt", "backup", "--preset", "mnemosyne", "--keep-days", "1", b.dir)
	log := e.must(b.remote, "git", "log", "--format=%s")
	if subject, _, _ := strings.Cut(log, "\n"); subject != "salt backup" || strings.Contains(log, "Set up salt") {
		t.Fatalf("the remote's history is:\n%s", log)
	}
}

// salt backup refuses to back up with no branch checked out, before it
// commits anything, and a commit that git refuses, here because signing it
// fails, fails with git's own reason, not a bare exit status.
func TestBackupGitStepFailures(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "x"), "a file")
	before := commitCount(e, b.dir)
	e.must(b.dir, "git", "checkout", "-q", "--detach")
	out, code := e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "no branch is checked out (detached HEAD)") || commitCount(e, b.dir) != before {
		t.Fatalf("detached: exit %d:\n%s", code, out)
	}

	e.must(b.dir, "git", "checkout", "-q", "-")
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "y"), "another file")
	// salt's own commits are never signed, so a signing key that cannot be
	// used, as under cron, does not stop the backup.
	e.must(b.dir, "git", "config", "commit.gpgsign", "true")
	e.must(b.dir, "git", "config", "gpg.program", "false")
	head := e.must(b.dir, "git", "rev-parse", "HEAD")
	if out, code := e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir); code != 0 || e.must(b.dir, "git", "rev-parse", "HEAD") == head {
		t.Fatalf("signing on: exit %d:\n%s", code, out)
	}
	if sig := strings.TrimSpace(e.must(b.dir, "git", "log", "-1", "--format=%G?")); sig != "N" {
		t.Fatalf("salt's commit has signature status %q, want none", sig)
	}

	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "z"), "a third file")
	e.must(b.dir, "git", "config", "commit.cleanup", "bogus")
	out, code = e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "committing the backup: git commit:") || !strings.Contains(out, "bogus") {
		t.Fatalf("commit: exit %d:\n%s", code, out)
	}
}
