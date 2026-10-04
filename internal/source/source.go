package source

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// waitTimeout is how long a copy waits for another program's write or lock to
// be released before giving up.
const waitTimeout = 30 * time.Second

// Database is a live database that salt copies safely before sealing it.
// Each kind of database (see Kinds) implements it, so salt's commands work the
// same way for all of them.
type Database interface {
	// Name is the slash path the copy is backed up under. Each kind picks a
	// file name at the top of the backup; Named replaces it.
	Name() string
	// String shows the database in messages. It never holds a password.
	String() string
	// Flag is the option the database was given with, such as "--sqlite".
	Flag() string
	// Copy writes a consistent copy of the database to o.Dst while other
	// programs may be using it, and returns what to record for the copy.
	Copy(ctx context.Context, o CopyOptions) (Meta, error)
}

// CopyOptions describes one copy.
type CopyOptions struct {
	// Dst is the file to write. Its folder is private to salt.
	Dst string
	// Key is a secret salt keeps for the backup repo, the same on every run.
	// A copy uses it wherever its program would otherwise write something
	// random, so that an unchanged database gives an identical copy.
	Key string
}

// Meta is what the backup records for a copy in place of the copy's own
// permissions and last-modified date.
type Meta struct {
	Mode    fs.FileMode
	ModTime time.Time
}

// Kind is a way to give salt a live database on the command line.
type Kind struct {
	// Flag is the option's name, without dashes.
	Flag  string
	Usage string
	// New makes the database from the option's value. Its errors never
	// repeat a password.
	New func(arg string) (Database, error)
}

// Kinds lists every kind of database salt can copy. Adding a kind here adds
// its option to salt seal.
var Kinds = []Kind{
	{Flag: "sqlite", Usage: "a live SQLite database file to copy safely and seal", New: NewSQLite},
	{Flag: "postgres", Usage: "a Postgres connection URL or string to dump safely and seal", New: NewPostgres},
	{Flag: "postgres-env", Usage: "an environment variable holding a Postgres connection", New: NewPostgresEnv},
}

// named is a database backed up under a name the person chose.
type named struct {
	Database
	name string
}

func (n named) Name() string { return n.name }

// Named returns db backed up under name, a slash path in the backup, in place
// of the name db picks itself. It works the same for every kind, so two
// databases with the same name can both be backed up without renaming either.
// A name that could lead outside the backup is refused, and so is one ending
// in "/", which names a folder rather than the file to back the copy up as.
func Named(db Database, name string) (Database, error) {
	clean, err := repo.CleanPath(name)
	if err != nil || strings.HasSuffix(name, "/") {
		return nil, fmt.Errorf("the name %q cannot be used in the backup", name)
	}
	return named{Database: db, name: clean}, nil
}
