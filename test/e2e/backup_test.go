//go:build e2e

package e2e

import (
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
	write(t, filepath.Join(hermes, "mnemosyne", "blobs", "ab", "abcd", "abcdef"), "an attached image")
	write(t, filepath.Join(hermes, "mnemosyne", "models", "model.gguf"), "a downloaded model")
	write(t, filepath.Join(hermes, "mnemosyne", "logs", "diagnose.log"), "a log")
	write(t, filepath.Join(hermes, ".env"), "OPENAI_API_KEY=sk-secret")
	write(t, filepath.Join(e.home, ".mnemosyne", ".env"), "MNEMOSYNE_LLM_API_KEY=sk-secret")
	commits := commitCount(e, b.remote)

	out := e.must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	want := "salt: left ~/.hermes/profiles/coder/mnemosyne/config.yaml out of the backup because its setting llm_api_key holds a secret. Keep secrets in environment variables so the file can be backed up\n"
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

// salt backup stops, and says why, when the preset finds nothing, and when
// the push fails, without showing a password in origin's URL.
func TestBackupFailures(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	out, code := e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "found nothing to back up for the mnemosyne preset. It looks in ~/.hermes/mnemosyne/data,") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	before := commitCount(e, b.dir)

	write(t, filepath.Join(e.home, ".hermes", "mnemosyne", "blobs", "x"), "a file")
	e.must(b.dir, "git", "remote", "set-url", "origin", "https://agent:hunter2@127.0.0.1:1/backup.git")
	out, code = e.run(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if code != 1 || !strings.Contains(out, "the backup was committed but not pushed: git ls-remote") || strings.Contains(out, "hunter2") {
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
// so salt backup pushes on top of it.
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
	e.must(b.base, "salt", "backup", "--preset", "mnemosyne", b.dir)
	if subject := strings.TrimSpace(e.must(b.remote, "git", "log", "-1", "--format=%s")); subject != "salt backup" {
		t.Fatalf("the remote's latest commit is %q", subject)
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
