package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// header returns a database header whose bytes 18 and 19 are write and read.
func header(write, read byte) string {
	return sqliteHeader + "\x10\x00" + string([]byte{write, read}) + "pages"
}

func TestCheckSQLite(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		content string
		wal     bool
	}{
		"rollback": {header(1, 1), false},
		"wal":      {header(2, 2), true},
		"mixed":    {header(2, 1), false},
		"magic":    {sqliteHeader, false},
		"short":    {sqliteHeader + "\x10\x00\x02", false},
		"empty":    {"", false},
	} {
		wal, err := checkSQLite(writeFile(t, filepath.Join(dir, name), tc.content))
		if err != nil || wal != tc.wal {
			t.Errorf("%s: wal %v, %v; want %v", name, wal, err, tc.wal)
		}
	}
	for name, content := range map[string]string{
		"text":   "# MEMORY\n",
		"prefix": "SQLite",
		"close":  "SQLite format 3!pages",
	} {
		if _, err := checkSQLite(writeFile(t, filepath.Join(dir, name), content)); !errors.Is(err, ErrNotSQLite) {
			t.Errorf("%s: %v, want ErrNotSQLite", name, err)
		}
	}
	if _, err := checkSQLite(dir); !errors.Is(err, ErrNotSQLite) {
		t.Errorf("folder: %v, want ErrNotSQLite", err)
	}
	if _, err := checkSQLite(filepath.Join(dir, "missing.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v, want ErrNotExist", err)
	}
}

func TestCheckSQLiteUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	p := writeFile(t, filepath.Join(t.TempDir(), "locked.db"), sqliteHeader)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSQLite(p); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable: %v, want ErrPermission", err)
	}
}

// A named pipe is refused without opening it, which would wait forever.
func TestCheckSQLiteRefusesAPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe.db")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if _, err := checkSQLite(p); !errors.Is(err, ErrNotSQLite) {
		t.Fatalf("pipe: %v, want ErrNotSQLite", err)
	}
}

// CopySQLite checks the database before it starts sqlite3, so these fail
// without starting a process.
func TestCopySQLiteRefusesBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	db := writeFile(t, filepath.Join(dir, "live.db"), sqliteHeader)
	for name, tc := range map[string]struct {
		live, dst string
		want      string
	}{
		"missing":      {filepath.Join(dir, "missing.db"), filepath.Join(dir, "copy.db"), "no such file"},
		"not database": {writeFile(t, filepath.Join(dir, "notes.md"), "notes"), filepath.Join(dir, "copy.db"), ErrNotSQLite.Error()},
		"quoted name":  {db, filepath.Join(dir, "my copy.db"), "needs quoting"},
	} {
		err := CopySQLite(context.Background(), tc.live, tc.dst)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if _, err := os.Stat(tc.dst); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: a copy was made: %v", name, err)
		}
	}
}

// With sqlite3 missing, the command fails before any process starts.
func TestCopySQLiteWithoutSQLite3(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	db := writeFile(t, filepath.Join(dir, "live.db"), sqliteHeader)
	err := CopySQLite(context.Background(), db, filepath.Join(dir, "copy.db"))
	if !errors.Is(err, ErrMissingProgram) || err.Error() != "salt needs the sqlite3 program, which is not installed or not on PATH" {
		t.Fatalf("CopySQLite: %v", err)
	}
}

func TestSQLiteCommand(t *testing.T) {
	dir := t.TempDir()
	base := []string{"sqlite3", "-init", os.DevNull, "-bail", "-cmd", ".timeout 30000"}
	tail := []string{filepath.Join(dir, "-live.db"), ".backup 0.db"}
	for wal, extra := range map[bool][]string{
		false: nil,
		true:  {"-cmd", "BEGIN", "-cmd", "SELECT count(*) FROM sqlite_master"},
	} {
		cmd, err := sqliteCommand(context.Background(), filepath.Join(dir, "-live.db"), filepath.Join(dir, "tmp", "0.db"), wal)
		if err != nil {
			t.Fatal(err)
		}
		if want := slices.Concat(base, extra, tail); !slices.Equal(cmd.Args, want) {
			t.Fatalf("wal %v: args = %q, want %q", wal, cmd.Args, want)
		}
		if cmd.Dir != filepath.Join(dir, "tmp") {
			t.Fatalf("dir = %q", cmd.Dir)
		}
	}
	for _, name := range []string{"it's.db", "a b.db", `x"y.db`, "new\nline.db", "-x.db", ".hidden"} {
		if _, err := sqliteCommand(context.Background(), filepath.Join(dir, "live.db"), filepath.Join(dir, name), false); err == nil || !strings.Contains(err.Error(), "needs quoting") {
			t.Errorf("%q: %v", name, err)
		}
	}
	// A relative path would be read from the copy's folder.
	if _, err := sqliteCommand(context.Background(), "live.db", filepath.Join(dir, "0.db"), false); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("relative path: %v", err)
	}
}
