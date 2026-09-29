// Command salt encrypts AI-agent memory backups before they leave the machine.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spicy-lemonade/salt/internal/app"
	"github.com/spicy-lemonade/salt/internal/guard"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/seal"
)

var version = "dev"

const usage = `salt encrypts your agent's memory backups before they are pushed.

Setup:
  salt init REPO [--plain-paths] [--recovery phrase|passphrase] [--passphrase-file F]
      Set up salt in a git backup repo: create your key, choose how to
      recover it, and install the pre-commit hook.

Nightly (in your backup script):
  salt seal [--prune] SRC REPO
      Encrypt the snapshot directory SRC into REPO. Unchanged files are left
      untouched. --prune removes anything in REPO that salt did not write.
  salt check [REPO]
      Pre-commit hook: refuse the commit if any staged file is not encrypted.

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
	ui := app.NewTerminal()
	keyDir := filepath.Join(cfg, "salt", "keys")
	var store keys.Store = keys.FallbackStore{
		Primary:   keys.KeyringStore{},
		Secondary: keys.FileStore{Dir: keyDir},
		Warn: func(err error) {
			ui.Printf("salt: the system keychain is unavailable (%v); saving the key to a private file under %s instead\n", err, keyDir)
		},
	}
	name := storeName()
	// SALT_KEYSTORE=file skips the keychain: for headless machines, and for
	// the e2e tests, which must never touch the real keychain.
	if os.Getenv("SALT_KEYSTORE") == "file" {
		store, name = keys.FileStore{Dir: keyDir}, "private key file under "+keyDir
	}
	return &app.App{
		UI:        ui,
		Store:     store,
		StoreName: name,
		CacheDir:  cache,
		TrustDir:  filepath.Join(cfg, "salt", "trusted"),
		Git:       app.RealGit{},
		LookPath:  app.LookPath,
		Now:       time.Now,
		Version:   version,
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
		pos, err := parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		return a.Seal(pos[0], pos[1], *prune)
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
		return a.Restore(app.RestoreOptions{Repo: pos[0], To: *to, Paths: pos[1:], Force: *force})
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
			a.UI.Printf("✓ Installed %s\n", p)
		}
		return err
	}
	return usageError{fmt.Sprintf("unknown command %q", cmd)}
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
