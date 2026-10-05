package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/spicy-lemonade/salt/internal/proc"
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
	if !errors.Is(err, proc.ErrMissingProgram) || err.Error() != "salt needs the sqlite3 program, which is not installed or not on PATH" {
		t.Fatalf("CopySQLite: %v", err)
	}
}

// A copy sqlite3 did not make is errNoCopy, never fs.ErrNotExist, which
// would report the live database as missing. Any other error reading dst is
// returned as it is.
func TestCheckCopied(t *testing.T) {
	dir := t.TempDir()
	made := writeFile(t, filepath.Join(dir, "0"), sqliteHeader)
	if err := checkCopied(made); err != nil {
		t.Fatalf("a copy that exists: %v", err)
	}
	err := checkCopied(filepath.Join(dir, "1"))
	if !errors.Is(err, errNoCopy) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a copy that is missing: %v", err)
	}
	// A path through a regular file fails with "not a directory".
	err = checkCopied(filepath.Join(made, "2"))
	if err == nil || errors.Is(err, errNoCopy) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a copy that cannot be read: %v", err)
	}
}

// The database is opened as a URI with mode=rw, which sqlite3 never creates,
// so a database deleted after salt checked it is not made again, empty, in
// the tool's folder. Characters a URI gives a meaning to are escaped.
func TestSQLiteCommand(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "a b%?#", "-live.db")
	args := []string{"sqlite3", "-init", os.DevNull, "-bail", "file:" + filepath.ToSlash(dir) + "/a%20b%25%3F%23/-live.db?mode=rw"}
	// The commands go on stdin: with -bail, sqlite3 3.53 exits 0 without a
	// copy after the first SQL command given with -cmd.
	for wal, script := range map[bool]string{
		false: ".timeout 30000\n.backup 0.db\n",
		true:  ".timeout 30000\nBEGIN;\nSELECT count(*) FROM sqlite_master;\n.backup 0.db\n",
	} {
		cmd, err := sqliteCommand(context.Background(), live, filepath.Join(dir, "tmp", "0.db"), wal)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cmd.Args, args) {
			t.Fatalf("wal %v: args = %q, want %q", wal, cmd.Args, args)
		}
		if got, err := io.ReadAll(cmd.Stdin); err != nil || string(got) != script {
			t.Fatalf("wal %v: stdin = %q, %v, want %q", wal, got, err, script)
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

// A relative path is resolved from the current folder, since sqlite3 runs
// in the copy's folder.
func TestNewSQLite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	db, err := NewSQLite("memory.db")
	if err != nil {
		t.Fatal(err)
	}
	s := db.(*SQLite)
	if s.abs != filepath.Join(dir, "memory.db") || s.Name() != "memory.db" || s.String() != "memory.db" || s.Flag() != "--sqlite" {
		t.Fatalf("got %q %q %q %q", s.abs, s.Name(), s.String(), s.Flag())
	}
}

// Copy reports a missing database, or one that is not a SQLite database,
// before starting sqlite3.
func TestSQLiteCopyRefuses(t *testing.T) {
	dir := t.TempDir()
	for path, want := range map[string]error{
		filepath.Join(dir, "missing.db"):                          fs.ErrNotExist,
		writeFile(t, filepath.Join(dir, "notes.md"), "# notes\n"): ErrNotSQLite,
	} {
		db, _ := NewSQLite(path)
		if _, err := db.Copy(context.Background(), CopyOptions{Dst: filepath.Join(dir, "0")}); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", path, err, want)
		}
	}
}

// Without sqlite3, Copy fails before any process starts.
func TestSQLiteCopyWithoutSQLite3(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	db, _ := NewSQLite(writeFile(t, filepath.Join(dir, "live.db"), sqliteHeader))
	if _, err := db.Copy(context.Background(), CopyOptions{Dst: filepath.Join(dir, "0")}); !errors.Is(err, proc.ErrMissingProgram) {
		t.Fatalf("Copy: %v", err)
	}
}

// A relative path needs the current folder, which may be gone.
func TestNewSQLiteWithoutCurrentFolder(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	os.Mkdir(gone, 0o700)
	t.Chdir(gone)
	os.Remove(gone)
	if _, err := os.Getwd(); err == nil {
		t.Skip("this system still reports a removed current folder")
	}
	if _, err := NewSQLite("memory.db"); err == nil {
		t.Fatal("NewSQLite without a current folder succeeded")
	}
}

func TestIsSQLite(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]bool{
		sqliteHeader + "pages": true,
		sqliteHeader:           true,
		"":                     false,
		"SQLite":               false,
		"not a database file":  false,
	} {
		ok, err := IsSQLite(writeFile(t, filepath.Join(dir, "x"), content))
		if ok != want || err != nil {
			t.Errorf("IsSQLite(%q) = %v, %v", content, ok, err)
		}
	}
	if _, err := IsSQLite(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// A folder, or a named pipe that would block a read, is not a database,
	// and is never opened.
	if ok, err := IsSQLite(dir); ok || err != nil {
		t.Errorf("folder: %v, %v", ok, err)
	}
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := IsSQLite(pipe); ok || err != nil {
		t.Errorf("named pipe: %v, %v", ok, err)
	}
}
