//go:build e2e

package e2e

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setDate(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, time.Time{}, at); err != nil {
		t.Fatal(err)
	}
}

func assertDate(t *testing.T, path string, want time.Time) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(want) {
		t.Fatalf("%s last-modified = %v, want %v", filepath.Base(path), fi.ModTime(), want)
	}
}

func realSQLite(t *testing.T) string {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	return sqlite
}

// withTemp returns a copy of e whose salt makes its temporary files in a
// fresh folder, and that folder, so a test can check nothing is left there.
func withTemp(t *testing.T, e *env) (*env, string) {
	t.Helper()
	tmp := t.TempDir()
	return e.with("TMPDIR=" + tmp), tmp
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left in %s: %v", dir, left)
	}
}

// assertSealFails runs salt seal with args and checks it stops before
// sealing: exit 1, a message containing want, no database copy left behind
// and the repo unchanged. It returns the output.
func assertSealFails(t *testing.T, e *env, b *backupRepo, want string, args ...string) string {
	t.Helper()
	salt, tmp := withTemp(t, e)
	out, code := salt.run(b.base, "salt", append(append([]string{"seal"}, args...), b.src, b.dir)...)
	if code != 1 || !strings.HasPrefix(out, "salt: ") || !strings.Contains(out, want) {
		t.Fatalf("exit %d, want 1 and %q:\n%s", code, want, out)
	}
	assertEmpty(t, tmp)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("a failed copy changed the repo:\n%s", st)
	}
	return out
}

// assertInterruptStopsCopy runs salt seal with args, with a stand-in for
// program that runs script, then waits. Ctrl-C once the stand-in has started
// must stop it, remove the copy and seal nothing. script touches
// "$SALT_TEST_STARTED" once it has started copying.
func assertInterruptStopsCopy(t *testing.T, e *env, b *backupRepo, program, script string, args ...string) {
	t.Helper()
	bin := t.TempDir()
	started := filepath.Join(t.TempDir(), "started")
	if err := os.WriteFile(filepath.Join(bin, program), []byte("#!/bin/sh\n"+script+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	salt, tmp := withTemp(t, e)
	salt = salt.with("PATH="+bin+":"+filepath.Dir(e.bin)+":/usr/bin:/bin", "SALT_TEST_STARTED="+started)
	cmd := exec.Command(e.bin, append(append([]string{"seal"}, args...), b.src, b.dir)...)
	cmd.Env = salt.vars
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never started", program)
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 || !strings.Contains(out.String(), "seal interrupted: the database copies were removed and nothing was sealed") {
		t.Fatalf("exit %v, want 130:\n%s", err, out.String())
	}
	assertEmpty(t, tmp)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an interrupted seal changed the repo:\n%s", st)
	}
}

// startAgent holds the WAL-mode database db open in a sqlite3 process, as a
// running agent does, and commits rows rows that stay only in the -wal file.
// It returns once another connection can see them.
func startAgent(t *testing.T, e *env, sqlite, db string, rows int) {
	t.Helper()
	// WAL mode is stored in the database file, so it is set before the agent
	// starts. Switching to it while another connection reads fails with
	// "database is locked", and sqlite3 would carry on without a -wal file.
	e.must(filepath.Dir(db), sqlite, db, "PRAGMA journal_mode=wal; CREATE TABLE m(x);")
	agent := exec.Command(sqlite, db)
	agent.Env = e.vars
	in, err := agent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agent.Process.Kill()
		agent.Wait()
	})
	// Both connections wait for a lock rather than fail, and the agent stops
	// at its first error so a failed step can't go unnoticed.
	io.WriteString(in, ".bail on\n.timeout 10000\nPRAGMA wal_autocheckpoint=0;\n"+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<"+strconv.Itoa(rows)+") INSERT INTO m SELECT i FROM n;\n")
	// sqlite3 buffers its output to a pipe, so a second connection checks
	// when the rows are committed. It is not the last to close, so it leaves
	// the -wal file as it is.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := e.run(filepath.Dir(db), sqlite, "-cmd", ".timeout 10000", db, "SELECT count(*) FROM m;")
		if strings.TrimSpace(out) == strconv.Itoa(rows) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never committed its rows: %s", out)
		}
	}
}

// assertRestoredDB restores the backup and checks db opens, passes
// integrity_check and holds rows rows.
func assertRestoredDB(t *testing.T, e *env, sqlite string, b *backupRepo, name string, rows int) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored")
	e.must(b.base, "salt", "restore", b.dir, "--to", dest)
	p := filepath.Join(dest, name)
	out := e.must(dest, sqlite, p, "SELECT count(*) FROM m; PRAGMA integrity_check;")
	if f := strings.Fields(out); len(f) != 2 || f[0] != strconv.Itoa(rows) || f[1] != "ok" {
		t.Fatalf("restored %s holds:\n%s", name, out)
	}
	return p
}

// salt seal --sqlite copies a database while an agent holds it open, with
// its newest rows only in the -wal file. The restored database opens, passes
// integrity_check, holds every row and has the live file's date. The live
// database is left as it was, and no copy is left behind.
func TestSealLiveSQLite(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	live := filepath.Join(t.TempDir(), "my agent") // a space, to check quoting
	os.MkdirAll(live, 0o755)
	db := filepath.Join(live, "memory.db")
	startAgent(t, e, sqlite, db, 500)
	at := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	setDate(t, db, at)
	write(t, filepath.Join(b.src, "SOUL.md"), "be kind\n")

	salt, tmp := withTemp(t, e)
	salt.must(b.base, "salt", "seal", "--sqlite", db, b.src, b.dir)
	assertEmpty(t, tmp)
	assertDate(t, db, at)
	e.must(b.dir, "git", "add", "-A")
	e.must(b.dir, "git", "commit", "-q", "-m", "backup")
	if grep, _ := e.run(b.dir, "git", "grep", "-l", "-a", "SQLite format", "HEAD"); strings.TrimSpace(grep) != "" {
		t.Fatalf("plaintext database found in commit: %s", grep)
	}
	assertDate(t, assertRestoredDB(t, e, sqlite, b, "memory.db", 500), at)

	// A database that has not changed since makes no change to the repo.
	salt.must(b.base, "salt", "seal", "--sqlite", db, b.src, b.dir)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an unchanged database changed the repo:\n%s", st)
	}
}

// A database in rollback-journal mode, SQLite's default, is copied without a
// read transaction and restores intact.
func TestSealSQLiteRollbackMode(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	db := filepath.Join(t.TempDir(), "notes.db")
	out := e.must(b.base, sqlite, db, "CREATE TABLE m(x); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<300) INSERT INTO m SELECT i FROM n; PRAGMA journal_mode;")
	if strings.TrimSpace(out) != "delete" {
		t.Fatalf("journal mode = %q, want delete", out)
	}
	os.MkdirAll(b.src, 0o755)
	e.must(b.base, "salt", "seal", "--sqlite", db, b.src, b.dir)
	assertRestoredDB(t, e, sqlite, b, "notes.db", 300)
}

// A crash leaves changes only in the -wal file. sqlite3 recovers them into
// the copy, and moves them into the database when it closes, which re-dates
// the database. The backup still gets the date from before the copy.
func TestSealSQLiteAfterACrash(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	live := t.TempDir()
	startAgent(t, e, sqlite, filepath.Join(live, "state.db"), 500)
	// The agent's files are copied while it runs, which is what a crash
	// leaves.
	crashed := filepath.Join(t.TempDir(), "crashed")
	for _, name := range []string{"state.db", "state.db-wal"} {
		data, err := os.ReadFile(filepath.Join(live, name))
		if err != nil {
			t.Fatalf("the agent's %s: %v", name, err)
		}
		write(t, filepath.Join(crashed, name), string(data))
	}
	db := filepath.Join(crashed, "state.db")
	at := time.Date(2025, 7, 8, 9, 10, 11, 0, time.UTC)
	setDate(t, db, at)
	os.MkdirAll(b.src, 0o755)

	e.must(b.base, "salt", "seal", "--sqlite", db, b.src, b.dir)
	assertDate(t, assertRestoredDB(t, e, sqlite, b, "state.db", 500), at)
}

// Two agents that both keep a state.db are both backed up and restored when
// one is given another name with --name. Two with the same name are
// refused before sqlite3 runs. The second is in a folder whose name holds
// characters sqlite3 reads as part of a URI unless salt escapes them.
func TestSealSQLiteNamed(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	write(t, filepath.Join(b.src, "agent2", "SOUL.md"), "be brave\n")
	first := filepath.Join(t.TempDir(), "state.db")
	second := filepath.Join(t.TempDir(), "a b%20?#", "state.db")
	os.MkdirAll(filepath.Dir(second), 0o755)
	startAgent(t, e, sqlite, first, 100)
	startAgent(t, e, sqlite, second, 200)

	salt, tmp := withTemp(t, e)
	salt.must(b.base, "salt", "seal", "--sqlite", first, "--name", "agent2/state.db", "--sqlite", second, b.src, b.dir)
	assertEmpty(t, tmp)
	assertRestoredDB(t, e, sqlite, b, "state.db", 100)
	assertRestoredDB(t, e, sqlite, b, "agent2/state.db", 200)
	e.must(b.dir, "git", "add", "-A")
	e.must(b.dir, "git", "commit", "-q", "-m", "backup")

	// sqlite3 is never started for names that clash.
	assertSealFails(t, e.with("PATH="+filepath.Dir(e.bin)), b, "would both be backed up as state.db. Give one of them another name with --name NAME before its --sqlite",
		"--sqlite", first, "--sqlite", second)
}

// When the copy cannot be made, salt stops before sealing, says why, and
// leaves no copy behind.
func TestSealSQLiteFailures(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	e.must(dir, sqlite, db, "CREATE TABLE m(x);")
	notes := filepath.Join(dir, "notes.db")
	write(t, notes, "# not a database\n")
	failing := t.TempDir()
	if err := os.WriteFile(filepath.Join(failing, "sqlite3"), []byte("#!/bin/sh\necho \"Error: database is locked\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// sqlite3 3.53 once exited 0 without making a copy.
	noCopy := t.TempDir()
	if err := os.WriteFile(filepath.Join(noCopy, "sqlite3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saltOnly := "PATH=" + filepath.Dir(e.bin)
	for name, tc := range map[string]struct {
		db   string
		vars []string
		want string
	}{
		"missing database": {filepath.Join(dir, "gone.db"), nil, "gone.db does not exist; check the path given to --sqlite"},
		"not a database":   {notes, nil, "not a SQLite database"},
		"no sqlite3":       {db, []string{saltOnly}, "salt needs the sqlite3 program"},
		"sqlite3 fails":    {db, []string{"PATH=" + failing + ":" + filepath.Dir(e.bin) + ":/usr/bin:/bin"}, "database is locked"},
		"no copy made":     {db, []string{"PATH=" + noCopy + ":" + filepath.Dir(e.bin) + ":/usr/bin:/bin"}, "copying the database " + db + ": sqlite3 finished without making a copy"},
	} {
		t.Run(name, func(t *testing.T) {
			assertSealFails(t, e.with(tc.vars...), b, tc.want, "--sqlite", tc.db)
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.db")); !os.IsNotExist(err) {
		t.Fatalf("salt created the missing database: %v", err)
	}
}

// Ctrl-C while sqlite3 is copying stops it, removes the copy and seals
// nothing.
func TestSealSQLiteInterrupted(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	db := filepath.Join(t.TempDir(), "memory.db")
	write(t, db, "SQLite format 3\x00pages")
	assertInterruptStopsCopy(t, e, b, "sqlite3", `echo partial > "$PWD/0"; touch "$SALT_TEST_STARTED"`, "--sqlite", db)
}
