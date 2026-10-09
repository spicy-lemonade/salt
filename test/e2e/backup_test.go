//go:build e2e

package e2e

import (
	"fmt"
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

// salt backup --preset openclaw backs up each OpenClaw agent's workspace,
// with a SQLite database kept in it, the wiki, LanceDB memory, skills and
// settings, in OpenClaw's folder and in a profile's, while OpenClaw's own
// databases are in use. It leaves those databases out, as they hold its
// logins, and its credential files, sessions, logs, caches, the .env and
// key files in a workspace, and a settings file holding a token, but backs
// up one that only names where a token is kept, as a SecretRef. The
// restored files are the same and the restored database opens and holds
// every row. A second run with nothing changed makes no commit, and
// OPENCLAW_STATE_DIR names another OpenClaw folder. Which files the preset
// finds is tested in internal/preset.
func TestBackupOpenClaw(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	state := filepath.Join(e.home, ".openclaw")
	// onDisk returns where the file backed up at rel is: a profile's under
	// its own folder, and the rest under the home folder with a leading dot.
	onDisk := func(rel string) string {
		if r, ok := strings.CutPrefix(rel, "openclaw-profiles/work/"); ok {
			return filepath.Join(e.home, ".openclaw-work", r)
		}
		return filepath.Join(e.home, "."+rel)
	}
	if err := os.MkdirAll(filepath.Join(state, "agents", "main", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	startAgent(t, e, sqlite, filepath.Join(state, "agents", "main", "agent", "openclaw-agent.sqlite"), 100)
	makeDB(t, e, sqlite, filepath.Join(state, "state", "openclaw.sqlite"), 5)
	makeDB(t, e, sqlite, filepath.Join(state, "workspace", "notes.db"), 4)
	kept := map[string]string{
		"openclaw/workspace/AGENTS.md":                                "Read MEMORY.md first.",
		"openclaw/workspace/SOUL.md":                                  "You are Claw.",
		"openclaw/workspace/MEMORY.md":                                "The user deploys on Fridays.",
		"openclaw/workspace/memory/2026-10-07.md":                     "a daily note",
		"openclaw/workspace/skills/notes/SKILL.md":                    "a workspace skill",
		"openclaw/workspace/projects/plan.md":                         "the agent's own file",
		"openclaw/workspace-coder/MEMORY.md":                          "The repo uses Go.",
		"openclaw/wiki/main/index.md":                                 "a wiki page",
		"openclaw/memory/lancedb/memories.lance/data/0.lance":         "lance data",
		"openclaw/skills/shared/SKILL.md":                             "a shared skill",
		"openclaw/agents/main/agent/workshop-skills/learned/SKILL.md": "a learned skill",
		"openclaw-profiles/work/openclaw.json":                        `{"session": {"mainKey": "main"}, "channels": {"telegram": {"botToken": {"source": "env", "id": "TG_TOKEN"}}}}`,
		"openclaw-profiles/work/workspace/MEMORY.md":                  "work memory",
		"openclaw-profiles/work/workspace-ops/SOUL.md":                "ops soul",
	}
	for rel, content := range kept {
		write(t, onDisk(rel), content)
	}
	for _, rel := range []string{
		".env", "secrets.json", "gateway.token", "credentials/oauth.json", "identity/device.json",
		"logs/commands.log", "agents/main/sessions/s1.jsonl", "agents/main/agent/codex-home/auth.json",
		"workspace/.env", "workspace/deploy.pem", "workspace/.ssh/id_ed25519", "workspace/node_modules/x/index.js",
	} {
		write(t, filepath.Join(state, rel), "left out")
	}
	write(t, filepath.Join(e.home, ".openclaw-work", "credentials", "whatsapp", "creds.json"), "left out")
	write(t, filepath.Join(state, "openclaw.json"), `{"gateway": {"auth": {"token": "tok-secret"}}}`)
	commits := commitCount(e, b.remote)

	out := e.must(b.base, "salt", "backup", "--preset", "openclaw", b.dir)
	if want := "salt: left ~/.openclaw/openclaw.json out of the backup because its setting token holds a secret. Keep secrets in environment variables so the file can be backed up\n"; out != want {
		t.Fatalf("backup printed:\n%s", out)
	}
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	wantFiles := slices.Sorted(slices.Values(append(slices.Collect(maps.Keys(kept)), "openclaw/workspace/notes.db")))
	if !slices.Equal(files, wantFiles) {
		t.Fatalf("restored %v, want %v", files, wantFiles)
	}
	for rel, content := range kept {
		if got, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(got) != content {
			t.Errorf("restored %s holds %q, %v", rel, got, err)
		}
	}
	assertDB(t, e, sqlite, filepath.Join(dest, "openclaw/workspace/notes.db"), 4)
	// OpenClaw's live database is left in use as it was.
	if _, err := os.Stat(filepath.Join(state, "agents", "main", "agent", "openclaw-agent.sqlite-wal")); err != nil {
		t.Fatalf("the live -wal file: %v", err)
	}

	commits = commitCount(e, b.remote)
	e.must(b.base, "salt", "backup", "--preset", "openclaw", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	// OPENCLAW_STATE_DIR names the OpenClaw folder in place of ~/.openclaw.
	other := filepath.Join(t.TempDir(), "openclaw")
	write(t, filepath.Join(other, "workspace", "MEMORY.md"), "another OpenClaw")
	e.with("OPENCLAW_STATE_DIR="+other).must(b.base, "salt", "backup", "--preset", "openclaw", b.dir)
	want := []string{"openclaw-profiles/work/openclaw.json", "openclaw-profiles/work/workspace-ops/SOUL.md", "openclaw-profiles/work/workspace/MEMORY.md", "openclaw/workspace/MEMORY.md"}
	if _, files = restoredFiles(t, e, b); !slices.Equal(files, want) {
		t.Fatalf("restored with OPENCLAW_STATE_DIR %v, want %v", files, want)
	}
}

// salt backup --preset openviking backs up OpenViking's memory, resources
// and snapshot history, its client and workspace settings, and the same in
// the Hermes folder and a Hermes profile, with Hermes's record of the
// memories it copied. It leaves out the vector index, logs, locks, keys,
// logins, the Hermes server's own settings, runtime and models, and a
// settings file holding an API key. The restored files are the same, and a
// second run with nothing changed makes no commit. HERMES_HOME names
// another Hermes folder. Which files the preset finds is tested in
// internal/preset.
func TestBackupOpenViking(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	kept := map[string]string{
		"openviking/ovcli.conf":                                                           `{"url": "http://127.0.0.1:1933"}`,
		"openviking/workspaces/salt-1a2b.json":                                            `{"version": 1, "peer": {"id": "salt"}}`,
		"openviking/data/viking/default/_system/users.json":                               `{"users": {"default": {"role": "admin", "key": ""}}}`,
		"openviking/data/viking/default/user/default/memories/preferences/deploys.md":     "The user deploys on Fridays.",
		"openviking/data/viking/default/user/default/memories/.abstract.md":               "a summary",
		"openviking/data/viking/default/resources/log/notes.md":                           "a resource folder named log",
		"openviking/data/.ovgit/default/HEAD":                                             "ref: refs/heads/main\n",
		"hermes/openviking/memory_mirror_registry.json":                                   `{"entries": {}}`,
		"hermes/openviking/data/viking/default/user/default/memories/m.md":                "hermes memory",
		"hermes/profiles/coder/openviking/data/viking/default/user/default/memories/m.md": "coder memory",
	}
	for rel, content := range kept {
		write(t, filepath.Join(e.home, "."+rel), content)
	}
	for _, rel := range []string{
		".openviking/master.key", ".openviking/codex_auth.json", ".openviking/logs/cc-hooks.log",
		".openviking/data/.openviking.lock", ".openviking/data/vectordb/context/000001.sst", ".openviking/data/log/openviking.log",
		".openviking/data/viking/default/resources/log/.path.ovlock",
		".hermes/openviking/ov.conf", ".hermes/openviking/runtime/bin/openviking-server", ".hermes/openviking/models/bge.gguf",
		".hermes/openviking/pending_sessions/s.json",
	} {
		write(t, filepath.Join(e.home, rel), "left out")
	}
	write(t, filepath.Join(e.home, ".openviking", "ov.conf"), `{"embedding": {"dense": {"api_key": "sk-secret"}}}`)
	commits := commitCount(e, b.remote)

	out := e.must(b.base, "salt", "backup", "--preset", "openviking", b.dir)
	warning := "salt: left ~/.openviking/ov.conf out of the backup because its setting api_key holds a secret. Keep secrets in environment variables so the file can be backed up\n"
	if out != warning {
		t.Fatalf("backup printed:\n%s", out)
	}
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	if want := slices.Sorted(maps.Keys(kept)); !slices.Equal(files, want) {
		t.Fatalf("restored %v, want %v", files, want)
	}
	for rel, content := range kept {
		if got, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(got) != content {
			t.Errorf("restored %s holds %q, %v", rel, got, err)
		}
	}

	commits = commitCount(e, b.remote)
	e.must(b.base, "salt", "backup", "--preset", "openviking", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	// HERMES_HOME names the Hermes folder in place of ~/.hermes, and the
	// places only ~/.hermes held are named as missing.
	other := filepath.Join(t.TempDir(), "hermes")
	write(t, filepath.Join(other, "openviking", "data", "viking", "default", "user", "default", "memories", "m.md"), "another Hermes")
	missing := " was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n"
	if out := e.with("HERMES_HOME="+other).must(b.base, "salt", "backup", "--preset", "openviking", b.dir); out != warning+
		"salt: hermes/openviking/memory_mirror_registry.json"+missing+
		"salt: hermes/profiles/coder/openviking/data/viking"+missing {
		t.Fatalf("backup with HERMES_HOME printed:\n%s", out)
	}
	var want []string
	for rel := range kept {
		if strings.HasPrefix(rel, "openviking/") {
			want = append(want, rel)
		}
	}
	want = slices.Sorted(slices.Values(append(want, "hermes/openviking/data/viking/default/user/default/memories/m.md")))
	dest, files = restoredFiles(t, e, b)
	if !slices.Equal(files, want) {
		t.Fatalf("restored with HERMES_HOME %v, want %v", files, want)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "hermes/openviking/data/viking/default/user/default/memories/m.md")); err != nil || string(got) != "another Hermes" {
		t.Fatalf("restored the Hermes memory as %q, %v", got, err)
	}
}

// salt backup --preset honcho dumps the database DB_CONNECTION_URI names,
// written as Honcho writes it with its driver, and backs it up with
// Honcho's settings in Hermes, leaving out a profile's settings holding an
// API key, and never shows a password. The restored dump loads into a new
// database that matches the live one, and a second run with nothing
// changed makes no commit. A server that cannot be reached stops the
// backup with nothing committed or pushed, and the error says where the
// connection came from. The test always sets
// DB_CONNECTION_URI, so it never reaches a real Postgres on this machine.
// Which files the preset finds is tested in internal/preset.
func TestBackupHoncho(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE honcho")
	s.psql(t, "honcho", memorySQL)
	live := s.psql(t, "honcho", pgSnapshot)
	b := newBackupRepo(t, e)
	hermes := filepath.Join(e.home, ".hermes")
	write(t, filepath.Join(hermes, "honcho.json"), `{"workspace": "hermes", "peerName": "me"}`)
	write(t, filepath.Join(hermes, "profiles", "coder", "honcho.json"), `{"workspace": "hermes", "apiKey": "hch-s3cret"}`)
	port := strconv.Itoa(s.port)
	salt, tmp := withTemp(t, saltWithPg(e, s).with("DB_CONNECTION_URI=postgresql+psycopg://agent:pa%3Ass%40w0rd%20s3cret@127.0.0.1:"+port+"/honcho"))
	commits := commitCount(e, b.remote)

	out := salt.must(b.base, "salt", "backup", "--preset", "honcho", b.dir)
	if want := "salt: left ~/.hermes/profiles/coder/honcho.json out of the backup because its setting apiKey holds a secret. Keep secrets in environment variables so the file can be backed up\n"; out != want {
		t.Fatalf("backup printed:\n%s", out)
	}
	assertEmpty(t, tmp)
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	if !slices.Equal(files, []string{"hermes/honcho.json", "honcho/honcho.sql"}) {
		t.Fatalf("restored %v", files)
	}
	s.psql(t, "postgres", "CREATE DATABASE honcho_restored")
	s.e.must(dest, filepath.Join(s.bin, "psql"), "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-d", "honcho_restored", "-f", filepath.Join(dest, "honcho", "honcho.sql"))
	if got := s.psql(t, "honcho_restored", pgSnapshot); got != live {
		t.Fatalf("restored database = %q, want %q", got, live)
	}

	commits = commitCount(e, b.remote)
	salt.must(b.base, "salt", "backup", "--preset", "honcho", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	s.stop()
	local := commitCount(e, b.dir)
	out, code := salt.run(b.base, "salt", "backup", "--preset", "honcho", b.dir)
	if code != 1 || !strings.Contains(out, "copying the database postgresql://agent@127.0.0.1:"+port+"/honcho: pg_dump") ||
		!strings.HasSuffix(strings.TrimSpace(out), "The honcho preset read this connection from DB_CONNECTION_URI") || strings.Contains(out, "s3cret") || strings.Contains(out, "pa:ss") {
		t.Fatalf("server stopped: exit %d:\n%s", code, out)
	}
	if commitCount(e, b.dir) != local || commitCount(e, b.remote) != commits {
		t.Fatal("a backup without the database was committed or pushed")
	}
	assertEmpty(t, tmp)
}

// salt backup --preset hindsight dumps the database HINDSIGHT_API_DATABASE_URL
// names, with a second schema as Hindsight keeps for each tenant and its
// vectors when pgvector is installed, and backs it up with each agent's
// Hindsight settings, leaving out settings holding a token and the .env
// files beside them, and never shows a password. The restored dump loads
// into a new database that matches the live one, and a second run with
// nothing changed makes no commit. A server that cannot be reached stops
// the backup with nothing committed or pushed, and the error says where the
// connection came from. The test always sets HINDSIGHT_API_DATABASE_URL, so
// it never reaches a real Postgres on this machine. Which files the preset
// finds is tested in internal/preset.
func TestBackupHindsight(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE hindsight")
	s.psql(t, "hindsight", memorySQL+`CREATE SCHEMA tenant_a; CREATE TABLE tenant_a.banks (id text PRIMARY KEY, config jsonb); INSERT INTO tenant_a.banks VALUES ('agent', '{"mission": "remember"}');`)
	snapshot := pgSnapshot + " UNION ALL SELECT 0, id || config::text, 0 FROM tenant_a.banks"
	if _, code := s.e.run(s.data, filepath.Join(s.bin, "psql"), "-X", "-q", "-d", "hindsight", "-c", "CREATE EXTENSION vector"); code == 0 {
		s.psql(t, "hindsight", "CREATE TABLE tenant_a.embeddings (id int PRIMARY KEY, v vector(3)); INSERT INTO tenant_a.embeddings VALUES (1, '[1,2,3]'), (2, '[0.5,0,-1]');")
		snapshot += " UNION ALL SELECT id, v::text, 0 FROM tenant_a.embeddings"
	} else {
		t.Log("pgvector is not installed, so vectors are not tested")
	}
	live := s.psql(t, "hindsight", snapshot)
	b := newBackupRepo(t, e)
	own := filepath.Join(e.home, ".hindsight")
	write(t, filepath.Join(own, "claude-code.json"), `{"bankId": "agent"}`)
	write(t, filepath.Join(own, "codex.json"), `{"bankId": "agent", "hindsightApiToken": "hsk-s3cret"}`)
	write(t, filepath.Join(own, "config.env"), "HINDSIGHT_API_LLM_API_KEY=sk-s3cret\n")
	port := strconv.Itoa(s.port)
	salt, tmp := withTemp(t, saltWithPg(e, s).with("HINDSIGHT_API_DATABASE_URL="+s.url("hindsight")))
	commits := commitCount(e, b.remote)

	out := salt.must(b.base, "salt", "backup", "--preset", "hindsight", b.dir)
	if want := "salt: left ~/.hindsight/codex.json out of the backup because its setting hindsightApiToken holds a secret. Keep secrets in environment variables so the file can be backed up\n"; out != want {
		t.Fatalf("backup printed:\n%s", out)
	}
	assertEmpty(t, tmp)
	if got := commitCount(e, b.remote); got == commits {
		t.Fatal("the backup was not pushed")
	}
	dest, files := restoredFiles(t, e, b)
	if !slices.Equal(files, []string{"hindsight/claude-code.json", "hindsight/hindsight.sql"}) {
		t.Fatalf("restored %v", files)
	}
	s.psql(t, "postgres", "CREATE DATABASE hindsight_restored")
	s.e.must(dest, filepath.Join(s.bin, "psql"), "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-d", "hindsight_restored", "-f", filepath.Join(dest, "hindsight", "hindsight.sql"))
	if got := s.psql(t, "hindsight_restored", snapshot); got != live {
		t.Fatalf("restored database = %q, want %q", got, live)
	}

	commits = commitCount(e, b.remote)
	salt.must(b.base, "salt", "backup", "--preset", "hindsight", b.dir)
	if got := commitCount(e, b.remote); got != commits {
		t.Fatalf("an unchanged backup made a commit: %s, then %s", commits, got)
	}

	s.stop()
	local := commitCount(e, b.dir)
	out, code := salt.run(b.base, "salt", "backup", "--preset", "hindsight", b.dir)
	if code != 1 || !strings.Contains(out, "copying the database postgresql://agent@127.0.0.1:"+port+"/hindsight: pg_dump") ||
		!strings.HasSuffix(strings.TrimSpace(out), "The hindsight preset read this connection from HINDSIGHT_API_DATABASE_URL") || strings.Contains(out, "s3cret") || strings.Contains(out, "pa:ss") {
		t.Fatalf("server stopped: exit %d:\n%s", code, out)
	}
	if commitCount(e, b.dir) != local || commitCount(e, b.remote) != commits {
		t.Fatal("a backup without the database was committed or pushed")
	}
	assertEmpty(t, tmp)
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

// ls-remote lists every ref whose name ends in the branch's. Enough of them
// on origin to make its output longer than salt reads stop the backup
// before old backups are dropped, rather than reading as no branch there.
func TestBackupRefusesAFloodOfRefs(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	tip := strings.TrimSpace(e.must(b.remote, "git", "rev-parse", "main"))
	var refs strings.Builder
	for i := range 1200 {
		fmt.Fprintf(&refs, "create refs/heads/a%04d/refs/heads/main %s\n", i, tip)
	}
	if out, code := e.runInput(b.remote, refs.String(), "git", "update-ref", "--stdin"); code != 0 {
		t.Fatalf("update-ref: exit %d:\n%s", code, out)
	}
	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "x"), "a file")
	out, code := e.run(b.base, "salt", "backup", "--preset", "mnemosyne", "--keep-days", "1", b.dir)
	if code != 1 || !strings.Contains(out, "salt could not check origin, so old backups were not dropped and it was not pushed: git ls-remote printed more than 64 KiB") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if log := e.must(b.dir, "git", "log", "--format=%s"); !strings.Contains(log, "Set up salt") {
		t.Fatalf("old backups were dropped:\n%s", log)
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
