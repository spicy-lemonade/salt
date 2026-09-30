//go:build e2e

package e2e

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeSQLite stands in for `sqlite3 DB ".backup 'COPY'"`. It copies the file
// without its date, as a real .backup does, then dates the database and its
// -wal file "now", as a real sqlite3 can when it closes (macOS keeps an empty
// -wal; after a crash the log is moved into the database). Any other command
// is refused, so a change to the README's call is noticed.
const fakeSQLite = `#!/bin/sh
case "$2" in
  ".backup '"*"'") ;;
  *) echo "unexpected sqlite3 command: $2" >&2; exit 2 ;;
esac
copy=${2#".backup '"}
copy=${copy%"'"}
cp "$1" "$copy" || exit 1
touch "$1" "$1-wal"
`

// readmeCopyDB returns the copy_db function from the README's backup script,
// so the tests run exactly what people copy.
func readmeCopyDB(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^copy_db\(\) \{\n.*?^\}\n`).Find(b)
	if fn == nil {
		t.Fatal("README.md has no copy_db function")
	}
	return string(fn)
}

// copyDB runs the README's copy_db under the script's own shell options, with
// dir first on PATH, and returns its output and exit code. It also checks
// that copy_db leaves nothing in the temp folder, whether it worked or not.
func copyDB(t *testing.T, e *env, dir, db, copy string) (string, int) {
	t.Helper()
	script := "set -euo pipefail\n" + readmeCopyDB(t) + `copy_db "$1" "$2"` + "\n"
	tmp := t.TempDir()
	vars := []string{"TMPDIR=" + tmp}
	for _, v := range e.vars {
		if path, ok := strings.CutPrefix(v, "PATH="); ok {
			v = "PATH=" + dir + ":" + path
		}
		vars = append(vars, v)
	}
	cmd := exec.Command("/bin/bash", "-c", script, "copy_db", db, copy)
	cmd.Env = vars
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("copy_db left %v in the temp folder", left)
	}
	return string(out), code
}

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

func installFakeSQLite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sqlite3"), []byte(fakeSQLite), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// copy_db gives the copy the database file's date from before the backup,
// in either SQLite journal mode, even though the backup re-dates the
// database. A -wal file's date is never used: in WAL mode the database file
// can be older than its newest change, and salt keeps the date as SQLite
// leaves it.
func TestReadmeCopyDBKeepsDates(t *testing.T) {
	e := newEnv(t)
	fakeBin := installFakeSQLite(t)
	dbDate := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	for name, walDate := range map[string]time.Time{
		"rollback journal": {}, // no -wal file
		"wal newer":        time.Date(2025, 3, 2, 18, 30, 0, 0, time.UTC),
		"wal older":        time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "my agent") // a space, to check quoting
			db := filepath.Join(dir, "state.db")
			write(t, db, "SQLite format 3\x00pages")
			setDate(t, db, dbDate)
			if !walDate.IsZero() {
				write(t, db+"-wal", "log")
				setDate(t, db+"-wal", walDate)
			}
			copy := filepath.Join(dir, "stage copy", "state.db")
			os.MkdirAll(filepath.Dir(copy), 0o755)
			if out, code := copyDB(t, e, fakeBin, db, copy); code != 0 {
				t.Fatalf("copy_db: exit %d\n%s", code, out)
			}
			if b, err := os.ReadFile(copy); err != nil || string(b) != "SQLite format 3\x00pages" {
				t.Fatalf("copy = %q, %v", b, err)
			}
			assertDate(t, copy, dbDate)
		})
	}
}

// When sqlite3 fails, or the database is missing, set -e stops the backup
// script inside copy_db, before salt seal runs. No copy is dated or left
// behind, and copyDB checks the date stamp is removed.
func TestReadmeCopyDBStopsOnFailure(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	write(t, db, "SQLite format 3\x00pages")
	failing := t.TempDir()
	if err := os.WriteFile(filepath.Join(failing, "sqlite3"), []byte("#!/bin/sh\necho \"Error: database is locked\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ bin, db string }{
		"missing database": {installFakeSQLite(t), filepath.Join(dir, "missing.db")},
		"sqlite3 fails":    {failing, db},
	} {
		t.Run(name, func(t *testing.T) {
			copy := filepath.Join(t.TempDir(), "copy.db")
			if out, code := copyDB(t, e, tc.bin, tc.db, copy); code == 0 {
				t.Fatalf("copy_db succeeded:\n%s", out)
			}
			if _, err := os.Stat(copy); !os.IsNotExist(err) {
				t.Fatalf("a failed copy left %s behind: %v", copy, err)
			}
		})
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

// With the real sqlite3, copy_db makes a readable copy of a WAL-mode
// database with the database file's date.
func TestReadmeCopyDBWithRealSQLite(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "my agent")
	os.MkdirAll(dir, 0o755)
	db := filepath.Join(dir, "mnemosyne.db")
	if out, code := e.run(dir, sqlite, db, "PRAGMA journal_mode=wal; CREATE TABLE m(x); INSERT INTO m VALUES('remember this');"); code != 0 {
		t.Fatalf("creating the database: exit %d\n%s", code, out)
	}
	at := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	setDate(t, db, at)
	copy := filepath.Join(dir, "copy.db")
	if out, code := copyDB(t, e, filepath.Dir(sqlite), db, copy); code != 0 {
		t.Fatalf("copy_db: exit %d\n%s", code, out)
	}
	assertDate(t, copy, at)
	if out := e.must(dir, sqlite, copy, "SELECT x FROM m;"); strings.TrimSpace(out) != "remember this" {
		t.Fatalf("copy holds %q", out)
	}
}

// A crash leaves changes only in the -wal file. The real sqlite3 recovers
// them into the copy, and moves them into the database when it closes, which
// re-dates the database. The copy still gets the date from before the
// backup.
func TestReadmeCopyDBAfterACrash(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	live := t.TempDir()
	db := filepath.Join(live, "state.db")

	// An "agent" holds the database open with its changes only in the -wal
	// file. Its files are copied while it runs, which is what a crash leaves.
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
	io.WriteString(in, "PRAGMA journal_mode=wal;\nPRAGMA wal_autocheckpoint=0;\nCREATE TABLE m(x);\n"+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<500) INSERT INTO m SELECT i FROM n;\n")
	// sqlite3 buffers its output to a pipe, so a second connection checks
	// when the rows are committed. It is not the last to close, so it leaves
	// the -wal file as it is.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := e.run(live, sqlite, db, "SELECT count(*) FROM m;")
		if strings.TrimSpace(out) == "500" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never committed its rows: %s", out)
		}
	}
	crashed := filepath.Join(t.TempDir(), "crashed")
	for _, name := range []string{"state.db", "state.db-wal"} {
		b, err := os.ReadFile(filepath.Join(live, name))
		if err != nil {
			t.Fatalf("the agent's %s: %v", name, err)
		}
		write(t, filepath.Join(crashed, name), string(b))
	}
	crashedDB := filepath.Join(crashed, "state.db")
	at := time.Date(2025, 7, 8, 9, 10, 11, 0, time.UTC)
	setDate(t, crashedDB, at)

	copy := filepath.Join(t.TempDir(), "copy.db")
	if out, code := copyDB(t, e, filepath.Dir(sqlite), crashedDB, copy); code != 0 {
		t.Fatalf("copy_db: exit %d\n%s", code, out)
	}
	assertDate(t, copy, at)
	out := e.must(crashed, sqlite, copy, "SELECT count(*) FROM m; PRAGMA integrity_check;")
	if f := strings.Fields(out); len(f) != 2 || f[0] != "500" || f[1] != "ok" {
		t.Fatalf("copy after a crash holds:\n%s", out)
	}
}
