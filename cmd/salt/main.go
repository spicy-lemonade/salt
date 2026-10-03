// Command salt encrypts AI-agent memory backups before they leave the machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spicy-lemonade/salt/internal/app"
	"github.com/spicy-lemonade/salt/internal/guard"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
)

var version = "dev"

// gitOps is how salt reaches git. Tests replace it so they never start git.
var gitOps app.GitOps = app.RealGit{}

// databaseKinds are the kinds of live database salt seal can copy. Tests
// replace them so they never start a database program.
var databaseKinds = source.Kinds

const usage = `salt encrypts your agent's memory backups before they are pushed.

Setup:
  salt init REPO [--plain-paths] [--recovery phrase|passphrase] [--passphrase-file F]
      Set up salt in a git backup repo: create your key, choose how to
      recover it, and install the pre-commit hook.

Nightly (in your backup script):
  salt seal [--prune] [--sqlite DB]... [--postgres CONN]... [--postgres-env VAR]... SRC REPO
      Encrypt the snapshot directory SRC into REPO. Unchanged files are left
      untouched. --prune removes anything in REPO that salt did not write.
      Fails if git would ignore or change any file salt wrote.
      Each database option makes a safe copy of a live database, even while
      it is in use, and seals it at the top of the backup. Repeat them for
      each database.
      --sqlite copies the SQLite database file DB under its file name. Needs
      the sqlite3 program.
      --postgres dumps the Postgres database CONN names, as plain SQL, under
      its name with .sql. CONN is a URL such as
      postgresql://user@host:5432/dbname or settings such as
      "host=localhost dbname=memory". Needs the pg_dump program.
      --postgres-env does the same for the connection held by the
      environment variable VAR, so its password never shows in a process
      list. Without a password in the connection, pg_dump looks in ~/.pgpass
      or PGPASSWORD.
  salt prune [--keep-days N] REPO
      Keep only the backups from the last N days on which anything in REPO
      changed (default 5), counted over the whole repo, and drop older ones
      from the branch's history. The latest backup is always kept. Rewrites
      history, so push with git push --force-with-lease.
  salt check [REPO]
      Pre-commit hook: refuse the commit if any staged file is not encrypted,
      or if git would ignore or change any file salt wrote.

Restoring:
  salt restore REPO --to DIR [--force] [PATH...]
      Decrypt the backup (or only PATHs) into DIR.
  salt recovery test REPO
      Check your recovery phrase or passphrase opens this backup.
  salt recovery show REPO
      Show the recovery phrase saved on this machine.

Checking:
  salt verify REPO
      Decrypt every file (nothing is written to disk) and check it against
      the index. Needs your key.
  salt doctor [REPO]
      Check salt, the hook, the key and the repo are healthy. Needs no key.
  salt trust [--yes] REPO
      Approve the repo's keys and settings for backups from this machine.
      Needed after cloning a backup repo onto a new machine, or after you
      change its keys yourself.

Other:
  salt hook install [REPO]
  salt version

Environment:
  SALT_KEYSTORE=file   keep keys in a 0600 file instead of the system keychain
`

func main() {
	if err := guard.Enter(); err != nil {
		fmt.Fprintln(os.Stderr, "salt:", err)
		os.Exit(3)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	err := run(os.Args[1], os.Args[2:])
	var ue usageError
	switch {
	case err == nil:
	case errors.Is(err, app.ErrReported):
		os.Exit(1) // already reported
	case errors.Is(err, app.ErrInterrupted):
		fmt.Fprintln(os.Stderr, "salt:", err)
		os.Exit(130)
	case errors.As(err, &ue):
		fmt.Fprintf(os.Stderr, "salt: %v\n\n%s", err, usage)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "salt:", err)
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func newApp() (*app.App, error) {
	cache, err := seal.DefaultCacheDir()
	if err != nil {
		return nil, err
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	// Without a home folder, paths are simply shown in full.
	home, _ := os.UserHomeDir()
	ui := app.NewTerminal()
	keyDir := filepath.Join(cfg, "salt", "keys")
	var store keys.Store = keys.FallbackStore{
		Primary:   keys.KeyringStore{},
		Secondary: keys.FileStore{Dir: keyDir},
		Warn: func(err error) {
			ui.Printf("salt: the system keychain is unavailable (%v); saving the key to a private file under %s instead\n", err, app.ShortPath(keyDir, home))
		},
	}
	name := storeName()
	// SALT_KEYSTORE=file skips the keychain: for headless machines, and for
	// the e2e tests, which must never touch the real keychain.
	if os.Getenv("SALT_KEYSTORE") == "file" {
		store, name = keys.FileStore{Dir: keyDir}, "private key file under "+app.ShortPath(keyDir, home)
	}
	return &app.App{
		UI:        ui,
		Store:     store,
		StoreName: name,
		CacheDir:  cache,
		TrustDir:  filepath.Join(cfg, "salt", "trusted"),
		Git:       gitOps,
		LookPath:  app.LookPath,
		Now:       time.Now,
		Version:   version,
		Home:      home,
	}, nil
}

func storeName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	}
	return "system keyring"
}

func run(cmd string, args []string) error {
	switch cmd {
	case "version", "--version":
		fmt.Println("salt", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	a, err := newApp()
	if err != nil {
		return err
	}
	switch cmd {
	case "init":
		fs := newFlags("init")
		plain := fs.Bool("plain-paths", false, "keep file names visible in the repo")
		recovery := fs.String("recovery", "", "phrase or passphrase")
		passFile := fs.String("passphrase-file", "", "read the passphrase from this file")
		pos, err := parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.Init(app.InitOptions{Repo: pos[0], PlainPaths: *plain, Recovery: *recovery, PassphraseFile: *passFile})
	case "seal":
		fs := newFlags("seal")
		prune := fs.Bool("prune", false, "remove files in REPO that salt did not write")
		given := databaseFlags(fs)
		pos, err := parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		dbs, err := given.databases()
		if err != nil {
			return err
		}
		o := app.SealOptions{Src: pos[0], Repo: pos[1], Prune: *prune, Databases: dbs}
		// Only database copies need cleaning up after Ctrl-C or SIGTERM.
		// Without them, a signal stops salt at once, as it always has.
		if len(dbs) > 0 {
			ctx, stop := interruptible()
			defer stop()
			o.Context = ctx
		}
		return a.Seal(o)
	case "prune":
		fs := newFlags("prune")
		days := fs.Int("keep-days", prune.DefaultKeepDays, "days with a change to keep")
		pos, err := parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		if *days < 1 {
			return usageError{fmt.Sprintf("prune: --keep-days must be 1 or more, got %d", *days)}
		}
		return a.Prune(pos[0], *days)
	case "check":
		pos, err := parse(newFlags("check"), args, 0, 1)
		if err != nil {
			return err
		}
		return a.Check(orDot(pos))
	case "restore":
		fs := newFlags("restore")
		to := fs.String("to", "", "directory to restore into")
		force := fs.Bool("force", false, "restore over a non-empty directory (it is moved aside)")
		pos, err := parse(fs, args, 1, -1)
		if err != nil {
			return err
		}
		if *to == "" {
			return usageError{"restore needs --to DIR"}
		}
		// Ctrl-C or SIGTERM stops the restore and removes the partly
		// restored files.
		ctx, stop := interruptible()
		defer stop()
		return a.Restore(app.RestoreOptions{Repo: pos[0], To: *to, Paths: pos[1:], Force: *force, Context: ctx})
	case "recovery":
		if len(args) == 0 {
			return usageError{"recovery needs a subcommand: test or show"}
		}
		pos, err := parse(newFlags("recovery "+args[0]), args[1:], 1, 1)
		if err != nil {
			return err
		}
		switch args[0] {
		case "test":
			return a.RecoveryTest(pos[0])
		case "show":
			return a.RecoveryShow(pos[0])
		}
		return usageError{fmt.Sprintf("unknown recovery subcommand %q", args[0])}
	case "verify":
		pos, err := parse(newFlags("verify"), args, 1, 1)
		if err != nil {
			return err
		}
		return a.Verify(pos[0])
	case "doctor":
		pos, err := parse(newFlags("doctor"), args, 0, 1)
		if err != nil {
			return err
		}
		return a.Doctor(orDot(pos))
	case "trust":
		fs := newFlags("trust")
		yes := fs.Bool("yes", false, "approve without asking")
		pos, err := parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.Trust(pos[0], *yes)
	case "hook":
		if len(args) == 0 || args[0] != "install" {
			return usageError{"usage: salt hook install [REPO]"}
		}
		pos, err := parse(newFlags("hook install"), args[1:], 0, 1)
		if err != nil {
			return err
		}
		p, err := a.InstallHook(orDot(pos))
		if err == nil {
			a.UI.Printf("✓ Installed %s\n", app.ShortPath(p, a.Home))
		}
		return err
	}
	return usageError{fmt.Sprintf("unknown command %q", cmd)}
}

// givenDatabase is a database option's value and the kind it makes.
type givenDatabase struct {
	kind source.Kind
	arg  string
}

type givenDatabases []givenDatabase

// databaseFlags adds an option to fs for each kind of database, which may be
// repeated, and returns the values given, in order.
func databaseFlags(fs *flag.FlagSet) *givenDatabases {
	given := &givenDatabases{}
	for _, k := range databaseKinds {
		fs.Func(k.Flag, k.Usage, func(arg string) error {
			*given = append(*given, givenDatabase{kind: k, arg: arg})
			return nil
		})
	}
	return given
}

// databases makes the databases only after parsing, so a value that holds a
// password never shows in a usage error.
func (g *givenDatabases) databases() ([]source.Database, error) {
	var dbs []source.Database
	for _, d := range *g {
		db, err := d.kind.New(d.arg)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, db)
	}
	return dbs, nil
}

// interruptible returns a context that Ctrl-C or SIGTERM cancels, so the
// command can remove the files it wrote. A second signal quits at once.
func interruptible() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse accepts flags before, between and after positional arguments and
// checks the positional count (max < 0 means unlimited).
func parse(fs *flag.FlagSet, args []string, minN, maxN int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{fmt.Sprintf("%s: %v", fs.Name(), err)}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) < minN || (maxN >= 0 && len(pos) > maxN) {
		return nil, usageError{fmt.Sprintf("%s: wrong number of arguments", fs.Name())}
	}
	return pos, nil
}

func orDot(pos []string) string {
	if len(pos) == 0 {
		return "."
	}
	return pos[0]
}
