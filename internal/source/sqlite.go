package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// sqliteHeader starts every SQLite database file.
const sqliteHeader = "SQLite format 3\x00"

// ErrNotSQLite means a file given as a SQLite database is not one.
var ErrNotSQLite = errors.New("not a SQLite database")

// errNoCopy means sqlite3 succeeded but wrote no copy. It is not
// fs.ErrNotExist, which would report the live database as missing.
var errNoCopy = errors.New("sqlite3 finished without making a copy")

// copyName limits the copy's file name to characters sqlite3 never needs
// quoted. It also keeps the name to one line of the commands sqlite3 reads.
var copyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// walVersion in header bytes 18 and 19 marks a database in WAL mode.
const walVersion = 2

// SQLite is a live SQLite database file.
type SQLite struct {
	given, abs string
}

// NewSQLite returns the SQLite database at path. A relative path is resolved
// from the current folder.
func NewSQLite(path string) (Database, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &SQLite{given: path, abs: abs}, nil
}

func (s *SQLite) Name() string   { return filepath.Base(s.abs) }
func (s *SQLite) String() string { return s.given }
func (s *SQLite) Flag() string   { return "--sqlite" }

// Copy copies the database with CopySQLite. The copy keeps the live file's
// permissions and last-modified date, read before copying, because copying
// can change them: after a crash, sqlite3 moves a leftover -wal file into the
// database when it closes.
func (s *SQLite) Copy(ctx context.Context, o CopyOptions) (Meta, error) {
	fi, err := os.Stat(s.abs)
	if err != nil {
		return Meta{}, err
	}
	if err := CopySQLite(ctx, s.abs, o.Dst); err != nil {
		return Meta{}, err
	}
	return Meta{Mode: fi.Mode(), ModTime: fi.ModTime()}, nil
}

// checkSQLite refuses a path that is not a regular file holding a SQLite
// database, and reports whether the database is in WAL mode. An empty file
// counts, since SQLite treats it as an empty database. Without this check,
// sqlite3 would create a new, empty database at a missing path and back that
// up.
func checkSQLite(path string) (wal bool, err error) {
	head, size, err := readHead(path)
	if err != nil {
		return false, err
	}
	if size == 0 {
		return false, nil
	}
	if !hasHeader(head) {
		return false, ErrNotSQLite
	}
	return len(head) == headLen && head[18] == walVersion && head[19] == walVersion, nil
}

// IsSQLite reports whether path is a regular file that starts with SQLite's
// header. Unlike checkSQLite, an empty file does not count, so only files
// that are already databases are found. Anything but a regular file is not
// a database.
func IsSQLite(path string) (bool, error) {
	head, _, err := readHead(path)
	if errors.Is(err, ErrNotSQLite) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hasHeader(head), nil
}

// headLen is how much of a database file readHead reads: the header string
// and the bytes up to the WAL mode marks.
const headLen = 20

// readHead returns as much of the first headLen bytes of the regular file at
// path as could be read, and its size. Anything but a regular file is
// ErrNotSQLite. The type is checked before opening: opening a named pipe
// would wait forever for a writer. A short file or a read error leaves fewer
// bytes than the header, and the file is not taken for a database. sqlite3
// would fail on the same read error.
func readHead(path string) (head []byte, size int64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, ErrNotSQLite
	}
	if fi.Size() == 0 {
		return nil, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	head = make([]byte, headLen)
	n, _ := io.ReadFull(f, head)
	return head[:n], fi.Size(), nil
}

// hasHeader reports whether head starts with SQLite's header.
func hasHeader(head []byte) bool {
	return len(head) >= len(sqliteHeader) && string(head[:len(sqliteHeader)]) == sqliteHeader
}

// CopySQLite writes a consistent copy of the SQLite database at live to dst
// with SQLite's own backup, which is safe while other programs write to the
// database. live must be an absolute path. sqlite3 copies it page by page,
// so it is never held in memory.
// dst's file name must start with a letter or digit and may only use
// letters, digits, '.', '_' and '-'. dst must not exist yet, so a copy
// that sqlite3 did not make is never taken for one it did.
func CopySQLite(ctx context.Context, live, dst string) error {
	wal, err := checkSQLite(live)
	if err != nil {
		return err
	}
	cmd, err := sqliteCommand(ctx, live, dst, wal)
	if err != nil {
		return err
	}
	if err := proc.Run(ctx, cmd); err != nil {
		return err
	}
	return checkCopied(dst)
}

// checkCopied returns errNoCopy if sqlite3 exited 0 without writing dst.
func checkCopied(dst string) error {
	switch _, err := os.Lstat(dst); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return errNoCopy
	default:
		return err
	}
}

// sqliteCommand builds the sqlite3 command that backs up live into dst. It
// runs in dst's folder, so the copy is named without a path. live must be an
// absolute path, so it does not depend on the folder sqlite3 runs in. It is
// opened as a URI (see sqliteURI), never mistaken for an option. -init skips
// the person's ~/.sqliterc.
//
// The commands are sent on stdin, one per line. With -bail, a failing command
// stops sqlite3 with an error. Some versions (3.53) stop after the first SQL
// command given with -cmd when -bail is set, exiting 0 without a copy.
//
// The backup copies a few pages at a time and starts again whenever another
// program saves to the database, so a large, busy database might never finish.
// In WAL mode, sqlite3 first opens a read transaction: the backup then copies
// the database as it was at that moment and never starts again, and other
// programs carry on saving to the -wal file. In rollback-journal mode, a read
// transaction would make other programs wait to save until the backup ends, so
// it is left out there.
func sqliteCommand(ctx context.Context, live, dst string, wal bool) (*exec.Cmd, error) {
	if !filepath.IsAbs(live) {
		return nil, fmt.Errorf("database path %q is not absolute", live)
	}
	name := filepath.Base(dst)
	if !copyName.MatchString(name) {
		return nil, fmt.Errorf("copy name %q needs quoting", name)
	}
	script := ".timeout " + strconv.FormatInt(waitTimeout.Milliseconds(), 10) + "\n"
	if wal {
		// A read starts the transaction; its output goes nowhere.
		script += "BEGIN;\nSELECT count(*) FROM sqlite_master;\n"
	}
	script += ".backup " + name + "\n"
	cmd := exec.CommandContext(ctx, "sqlite3", "-init", os.DevNull, "-bail", sqliteURI(live))
	cmd.Dir = filepath.Dir(dst)
	cmd.Stdin = strings.NewReader(script)
	return cmd, nil
}

// sqliteURI returns the URI that opens the database at the absolute path
// live with mode=rw. sqlite3 otherwise creates a database that is missing,
// so one the tool deletes after salt checks it would be made again, empty,
// in the tool's folder. Characters a URI gives a meaning to, such as ? and
// %, are escaped.
func sqliteURI(live string) string {
	p := filepath.ToSlash(live)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows path such as C:/x, which SQLite takes as /C:/x
	}
	return "file:" + (&url.URL{Path: p}).EscapedPath() + "?mode=rw"
}
