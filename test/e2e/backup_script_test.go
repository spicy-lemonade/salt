//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeSQLite stands in for `sqlite3 DB ".backup 'COPY'"`. It copies the file
// without its date, as a real .backup does, and refuses any other command so
// a change to the README's call is noticed.
const fakeSQLite = `#!/bin/sh
case "$2" in
  ".backup '"*"'") ;;
  *) echo "unexpected sqlite3 command: $2" >&2; exit 2 ;;
esac
copy=${2#".backup '"}
copy=${copy%"'"}
cp "$1" "$copy"
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
// dir first on PATH, and returns its output and exit code.
func copyDB(t *testing.T, e *env, dir, db, copy string) (string, int) {
	t.Helper()
	script := "set -euo pipefail\n" + readmeCopyDB(t) + `copy_db "$1" "$2"` + "\n"
	vars := make([]string, 0, len(e.vars))
	for _, v := range e.vars {
		if path, ok := strings.CutPrefix(v, "PATH="); ok {
			v = "PATH=" + dir + ":" + path
		}
		vars = append(vars, v)
	}
	cmd := exec.Command("/bin/bash", "-c", script, "copy_db", db, copy)
	cmd.Env = vars
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func setDate(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, time.Time{}, at); err != nil {
		t.Fatal(err)
	}
}

// copy_db gives the copy the date the database last changed, in both SQLite
// journal modes: the database file's own date, or the -wal file's date when
// the database is in WAL mode and that file is newer.
func TestReadmeCopyDBKeepsDates(t *testing.T) {
	e := newEnv(t)
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "sqlite3"), []byte(fakeSQLite), 0o755); err != nil {
		t.Fatal(err)
	}
	older := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2025, 3, 2, 18, 30, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		dbDate  time.Time
		walDate time.Time // zero: no -wal file
		want    time.Time
	}{
		"rollback journal":    {dbDate: older, want: older},
		"wal newer":           {dbDate: older, walDate: newer, want: newer},
		"wal older":           {dbDate: newer, walDate: older, want: newer},
		"wal and db the same": {dbDate: older, walDate: older, want: older},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "my agent") // a space, to check quoting
			db := filepath.Join(dir, "state.db")
			write(t, db, "SQLite format 3\x00pages")
			setDate(t, db, tc.dbDate)
			if !tc.walDate.IsZero() {
				write(t, db+"-wal", "log")
				setDate(t, db+"-wal", tc.walDate)
			}
			copy := filepath.Join(dir, "stage copy", "state.db")
			os.MkdirAll(filepath.Dir(copy), 0o755)
			if out, code := copyDB(t, e, fakeBin, db, copy); code != 0 {
				t.Fatalf("copy_db: exit %d\n%s", code, out)
			}
			if b, err := os.ReadFile(copy); err != nil || string(b) != "SQLite format 3\x00pages" {
				t.Fatalf("copy = %q, %v", b, err)
			}
			fi, err := os.Stat(copy)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.ModTime().Equal(tc.want) {
				t.Fatalf("copy last-modified = %v, want %v", fi.ModTime(), tc.want)
			}
		})
	}
}

// When sqlite3 fails, set -e stops the backup script inside copy_db, before
// salt seal runs, and no copy is dated or left behind.
func TestReadmeCopyDBStopsOnFailure(t *testing.T) {
	e := newEnv(t)
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "sqlite3"), []byte(fakeSQLite), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copy := filepath.Join(dir, "copy.db")
	if out, code := copyDB(t, e, fakeBin, filepath.Join(dir, "missing.db"), copy); code == 0 {
		t.Fatalf("copy_db of a missing database succeeded:\n%s", out)
	}
	if _, err := os.Stat(copy); !os.IsNotExist(err) {
		t.Fatalf("a failed copy left %s behind: %v", copy, err)
	}
}

// With the real sqlite3, copy_db makes a readable copy of a WAL-mode
// database dated when it last changed. sqlite3 folds the -wal file back in
// when it closes, so only the fake above can hold a -wal file open.
func TestReadmeCopyDBWithRealSQLite(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
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
	fi, err := os.Stat(copy)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(at) {
		t.Fatalf("copy last-modified = %v, want %v", fi.ModTime(), at)
	}
	if out := e.must(dir, sqlite, copy, "SELECT x FROM m;"); strings.TrimSpace(out) != "remember this" {
		t.Fatalf("copy holds %q", out)
	}
}
