package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// sqliteHeader starts every SQLite database file.
const sqliteHeader = "SQLite format 3\x00"

// busyTimeout is how long sqlite3 waits for another program's write to finish
// before giving up.
const busyTimeout = 30 * time.Second

// ErrNotSQLite means a file given as a SQLite database is not one.
var ErrNotSQLite = errors.New("not a SQLite database")

// copyName limits the copy's file name to characters sqlite3 never needs
// quoted.
var copyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// walVersion in header bytes 18 and 19 marks a database in WAL mode.
const walVersion = 2

// checkSQLite refuses a path that is not a regular file holding a SQLite
// database, and reports whether the database is in WAL mode. An empty file
// counts, since SQLite treats it as an empty database. Without this check,
// sqlite3 would create a new, empty database at a missing path and back that
// up.
func checkSQLite(path string) (wal bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, ErrNotSQLite
	}
	head := make([]byte, 20)
	n, err := io.ReadFull(f, head)
	if n == 0 && errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	if n < len(sqliteHeader) || string(head[:len(sqliteHeader)]) != sqliteHeader {
		return false, ErrNotSQLite
	}
	return n == len(head) && head[18] == walVersion && head[19] == walVersion, nil
}

// CopySQLite writes a consistent copy of the SQLite database at live to dst
// with SQLite's own backup, which is safe while other programs write to the
// database. sqlite3 copies it page by page, so it is never held in memory.
// dst's file name must start with a letter or digit and may only use
// letters, digits, '.', '_' and '-'.
func CopySQLite(ctx context.Context, live, dst string) error {
	wal, err := checkSQLite(live)
	if err != nil {
		return err
	}
	cmd, err := sqliteCommand(ctx, live, dst, wal)
	if err != nil {
		return err
	}
	return run(ctx, cmd)
}

// sqliteCommand builds the sqlite3 command that backs up live into dst. It
// runs in dst's folder, so the copy is named without a path. The database is
// passed by absolute path, so it is never mistaken for an option. -init skips
// the person's ~/.sqliterc.
//
// The backup copies a few pages at a time and starts again whenever another
// program saves to the database, so a large, busy database might never finish.
// In WAL mode, sqlite3 first opens a read transaction: the backup then copies
// the database as it was at that moment and never starts again, and other
// programs carry on saving to the -wal file. In rollback-journal mode, a read
// transaction would make other programs wait to save until the backup ends, so
// it is left out there.
func sqliteCommand(ctx context.Context, live, dst string, wal bool) (*exec.Cmd, error) {
	name := filepath.Base(dst)
	if !copyName.MatchString(name) {
		return nil, fmt.Errorf("copy name %q needs quoting", name)
	}
	abs, err := filepath.Abs(live)
	if err != nil {
		return nil, err
	}
	args := []string{"-init", os.DevNull, "-bail", "-cmd", ".timeout " + strconv.FormatInt(busyTimeout.Milliseconds(), 10)}
	if wal {
		// A read starts the transaction; its output goes nowhere.
		args = append(args, "-cmd", "BEGIN", "-cmd", "SELECT count(*) FROM sqlite_master")
	}
	cmd := exec.CommandContext(ctx, "sqlite3", append(args, abs, ".backup "+name)...)
	cmd.Dir = filepath.Dir(dst)
	return cmd, nil
}
