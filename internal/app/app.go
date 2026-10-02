// Package app implements salt's commands on top of the core packages. It talks
// to the person only through UI and reaches git only through injected
// functions, so unit tests start no processes.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// App holds salt's dependencies.
type App struct {
	UI    UI
	Store keys.Store
	// StoreName names where keys are kept, for messages ("macOS Keychain").
	StoreName string
	CacheDir  string
	// TrustDir holds this machine's approved keys and settings per repo.
	TrustDir string
	Git      GitOps
	// LookPath finds an executable the way the pre-commit hook would.
	LookPath func(name string) (string, bool)
	// CopySQLite makes a safe copy of a live SQLite database (see
	// source.CopySQLite). Tests replace it so they never start sqlite3.
	CopySQLite func(ctx context.Context, live, dst string) error
	Now        func() time.Time
	Version    string
	// Home is the user's home folder. Messages show paths inside it as "~/…";
	// empty means paths are shown in full.
	Home string
}

// GitOps is what salt asks of git. RealGit implements it; tests use a fake.
type GitOps interface {
	HookPath(repoRoot string) (string, error)
	Staged(repoRoot string) ([]check.Violation, error)
	Committed(repoRoot string) ([]check.Violation, error)
	LastCommit(repoRoot string) (t time.Time, ok bool, err error)
	Remote(repoRoot string) string
	// Storage reports files salt wrote that git would ignore or change when
	// storing them (see check.StorageProblems).
	Storage(repoRoot string) (problems []check.StorageProblem, total int, err error)
	// Prune drops backups older than the keepDays most recent days with a
	// change from the checked-out branch (see prune.Run).
	Prune(repoRoot string, keepDays int) (*prune.Result, error)
}

// RealGit runs git through gitx (hooks disabled).
type RealGit struct{}

func (RealGit) HookPath(root string) (string, error) { return gitx.HookPath(root, "pre-commit") }
func (RealGit) Staged(root string) ([]check.Violation, error) {
	return check.Staged(root)
}
func (RealGit) Committed(root string) ([]check.Violation, error) {
	return check.Committed(root)
}
func (RealGit) LastCommit(root string) (time.Time, bool, error) { return gitx.LastCommitTime(root) }
func (RealGit) Remote(root string) string                       { return gitx.Remote(root) }
func (RealGit) Storage(root string) ([]check.StorageProblem, int, error) {
	return check.StorageProblems(root)
}
func (RealGit) Prune(root string, keepDays int) (*prune.Result, error) {
	return prune.Run(prune.RealGit{}, root, keepDays)
}

// HookSearchPath is where the pre-commit hook looks for salt: the caller's
// PATH plus Homebrew's locations (see hook.Script).
var HookSearchPath = []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}

// LookPath finds an executable on $PATH or HookSearchPath without running it.
func LookPath(name string) (string, bool) {
	dirs := append(filepath.SplitList(os.Getenv("PATH")), HookSearchPath...)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// SealOptions configures Seal.
type SealOptions struct {
	Src, Repo string
	// Prune removes everything in Repo that salt did not write.
	Prune bool
	// SQLite lists live SQLite databases to copy safely and seal at the top
	// of the backup under their own file names.
	SQLite []string
	// Context stops the database copies early. Nil means never.
	Context context.Context
}

// Seal encrypts o.Src, and safe copies of o.SQLite, into the salt repository
// at o.Repo.
func (a *App) Seal(o SealOptions) error {
	r, err := repo.Open(o.Repo)
	if err != nil {
		return err
	}
	if err := a.checkTrusted(r); err != nil {
		return err
	}
	extra, cleanup, err := a.copyDatabases(o.Context, o.SQLite)
	if err != nil {
		return err
	}
	defer cleanup()
	res, err := seal.Seal(o.Src, r, seal.Options{CacheDir: a.CacheDir, Prune: o.Prune, Show: a.short, Extra: extra})
	if errors.Is(err, seal.ErrDuplicatePath) && len(extra) > 0 {
		return fmt.Errorf("%w; each --sqlite database is backed up under its file name, which must not be used by another database or by a file or folder at the top of %s", err, a.short(o.Src))
	}
	if err != nil {
		return err
	}
	// A signal during sealing lets it finish, so the copies are removed, but
	// still stops the backup script before it commits.
	if o.Context != nil && o.Context.Err() != nil {
		return fmt.Errorf("seal %w: the backup was sealed and the database copies were removed, but do not commit it without checking", ErrInterrupted)
	}
	a.UI.Printf("salt: sealed %d files and %d symlinks: %d encrypted, %d unchanged, %d removed\n",
		res.Files, res.Symlinks, res.Encrypted, res.Reused, len(res.Removed))
	for _, s := range res.Skipped {
		a.UI.Printf("salt: skipped %s (not a file or symlink)\n", s)
	}
	// Checked after sealing, not before: only now do the new objects exist,
	// so git can say whether it would ignore them. Failing here stops the
	// backup script before it commits.
	if a.requireGitRepo(r.Root) != nil {
		return nil // not a git repo, so nothing is pushed
	}
	return a.checkStorage(r.Root, "salt: the backup was sealed, but")
}

// copyDatabases makes a safe copy of each live database in a new private
// temporary folder, and returns them as files to seal with each database's
// own permissions and last-modified date. Those are read before the copy,
// because copying can change them: after a crash, sqlite3 moves a leftover
// -wal file into the database when it closes. cleanup removes the folder.
func (a *App) copyDatabases(ctx context.Context, dbs []string) (extra []seal.Extra, cleanup func(), err error) {
	if len(dbs) == 0 {
		return nil, func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tmp, err := os.MkdirTemp("", "salt-db-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	extra = make([]seal.Extra, len(dbs))
	for i, db := range dbs {
		dst := filepath.Join(tmp, strconv.Itoa(i)+".db")
		// sqlite3 runs in the temp folder, so it is given an absolute path.
		abs, err := filepath.Abs(db)
		var fi os.FileInfo
		if err == nil {
			fi, err = os.Stat(abs)
		}
		if err == nil {
			err = a.CopySQLite(ctx, abs, dst)
		}
		if err != nil {
			cleanup()
			switch {
			case ctx.Err() != nil:
				return nil, nil, fmt.Errorf("seal %w: the database copies were removed and nothing was sealed", ErrInterrupted)
			case errors.Is(err, fs.ErrNotExist):
				return nil, nil, fmt.Errorf("the database %s does not exist; check the path given to --sqlite", a.short(db))
			}
			return nil, nil, fmt.Errorf("copying the database %s: %w", a.short(db), err)
		}
		extra[i] = seal.Extra{Rel: filepath.Base(db), Path: dst, Mode: fi.Mode(), ModTime: fi.ModTime()}
	}
	return extra, cleanup, nil
}

// checkStorage refuses when git would not store salt's files as written.
func (a *App) checkStorage(root, lead string) error {
	problems, total, err := a.Git.Storage(root)
	if err != nil {
		return fmt.Errorf("checking how git will store the backup: %w", err)
	}
	if total > 0 {
		a.UI.Printf("%s", check.StorageReport(lead, problems, total))
		return ErrReported
	}
	return nil
}

// ErrReported means the command failed and has already told the person why.
var ErrReported = errors.New("problems reported")

// Check refuses plaintext staged in the repository at repoRoot.
func (a *App) Check(repoRoot string) error {
	vs, err := a.Git.Staged(repoRoot)
	if err != nil {
		return fmt.Errorf("salt check could not inspect the commit, refusing it: %w", err)
	}
	if len(vs) > 0 {
		a.UI.Printf("%s", check.Report(vs))
		return ErrReported
	}
	// Every tracked salt file, not only staged ones: a changed attribute also
	// breaks objects committed earlier, on the next fresh clone.
	return a.checkStorage(repoRoot, "salt check: refusing commit:")
}

// InstallHook installs the pre-commit hook in the repository at repoRoot.
func (a *App) InstallHook(repoRoot string) (string, error) {
	p, err := a.Git.HookPath(repoRoot)
	if err != nil {
		return "", err
	}
	err = hook.Install(p)
	if errors.Is(err, hook.ErrForeign) {
		err = fmt.Errorf("%w at %s; add `salt check` to it so plaintext commits are refused", err, a.short(p))
	}
	return p, err
}

func (a *App) requireGitRepo(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return fmt.Errorf("%s is not a git repository; clone or create your backup repo first", a.short(root))
	}
	return nil
}
