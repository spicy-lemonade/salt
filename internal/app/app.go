// Package app implements salt's commands on top of the core packages. It talks
// to the person only through UI and reaches git only through injected
// functions, so unit tests start no processes.
package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
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
	// SignDir holds this machine's keys for signing backups (see signStore).
	SignDir string
	Git     GitOps
	// LookPath finds an executable the way the pre-commit hook would.
	LookPath func(name string) (string, bool)
	Now      func() time.Time
	Version  string
	// Getenv reads an environment variable. Nil reads none, as if every
	// variable were unset.
	Getenv func(string) string
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
	// Clone downloads only the latest commit of url into the empty folder
	// dir (see gitx.Clone).
	Clone(ctx context.Context, url, dir string) error
	// Branch returns the checked-out branch, or gitx.ErrDetached.
	Branch(repoRoot string) (string, error)
	// Stage stages every change, removed files included.
	Stage(repoRoot string) error
	// Commit commits what is staged, and nothing when nothing is.
	Commit(ctx context.Context, repoRoot, msg string) error
	// Head returns the commit checked out.
	Head(repoRoot string) (string, error)
	// Push pushes the checked-out branch to origin if origin's branch is at
	// one of known, or nothing there is lost (see gitx.Push).
	Push(ctx context.Context, repoRoot string, known []string) error
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
func (RealGit) Clone(ctx context.Context, url, dir string) error {
	return gitx.Clone(ctx, url, dir)
}
func (RealGit) Branch(root string) (string, error) { return gitx.Branch(root) }
func (RealGit) Stage(root string) error            { return gitx.StageAll(root) }
func (RealGit) Commit(ctx context.Context, root, msg string) error {
	return gitx.CommitStaged(ctx, root, msg)
}
func (RealGit) Head(root string) (string, error) { return gitx.Head(root) }
func (RealGit) Push(ctx context.Context, root string, known []string) error {
	return gitx.Push(ctx, root, known)
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
	// Databases lists live databases to copy safely and seal, each under its
	// own name.
	Databases []source.Database
	// Files lists files to seal as they are, each under its own name.
	Files []seal.Extra
	// Context stops the database copies early. Nil means never.
	Context context.Context
}

// Seal encrypts o.Src, and safe copies of o.Databases, into the salt repository
// at o.Repo.
func (a *App) Seal(o SealOptions) error {
	r, signer, err := a.openToSeal(o.Repo)
	if err != nil {
		return err
	}
	res, err := a.seal(r, signer, o)
	if err != nil {
		return err
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

// openToSeal opens the salt repository at path for sealing. It refuses one
// whose keys or settings this machine has not approved, or with no key on
// this machine to sign its backups.
func (a *App) openToSeal(path string) (*repo.Repo, ed25519.PrivateKey, error) {
	r, err := repo.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if err := a.checkTrusted(r); err != nil {
		return nil, nil, err
	}
	signer, err := a.signingKey(r)
	if errors.Is(err, errNoSigningKey) {
		return nil, nil, fmt.Errorf("this machine has no key to sign backups to %s. salt now signs every backup, so restore can tell if someone planted files in it. Run `salt trust %q` once to set it up",
			a.short(r.Root), r.Root)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the signing key: %w", err)
	}
	return r, signer, nil
}

// seal copies o.Databases safely, then seals them, o.Src and o.Files into r.
func (a *App) seal(r *repo.Repo, signer ed25519.PrivateKey, o SealOptions) (*seal.Result, error) {
	extra, cleanup, err := a.copyDatabases(o.Context, r, o.Databases)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	res, err := seal.Seal(o.Src, r, seal.Options{CacheDir: a.CacheDir, Signer: signer, Prune: o.Prune, Show: a.short, Extra: slices.Concat(o.Files, extra)})
	if errors.Is(err, seal.ErrDuplicatePath) && len(extra) > 0 && o.Src != "" {
		return nil, fmt.Errorf("%w; each database salt copies is backed up under its own name, which must not be used by another database or by a file or folder in %s. Give the database another name with --name NAME before its option", err, a.short(o.Src))
	}
	if err != nil {
		return nil, err
	}
	// A signal during sealing lets it finish, so the copies are removed, but
	// still stops the backup script before it commits.
	if o.Context != nil && o.Context.Err() != nil {
		return nil, fmt.Errorf("seal %w: the backup was sealed and the database copies were removed, but do not commit it without checking", ErrInterrupted)
	}
	return res, nil
}

// copyDatabases makes a safe copy of each live database in a new private
// temporary folder, and returns them as files to seal. cleanup removes the
// folder.
func (a *App) copyDatabases(ctx context.Context, r *repo.Repo, dbs []source.Database) (extra []seal.Extra, cleanup func(), err error) {
	if len(dbs) == 0 {
		return nil, func() {}, nil
	}
	// Two databases whose names clash are refused before any copy is made,
	// since a copy can take a long time. A clash with a source file is only
	// known once seal reads the source.
	clashes := seal.ClashRule(r.Format.EncryptPaths)
	for i, db := range dbs {
		for _, prev := range dbs[:i] {
			if !clashes(prev.Name(), db.Name()) {
				continue
			}
			if prev.String() == db.String() && prev.Name() == db.Name() {
				return nil, nil, fmt.Errorf("the database %s is given twice. Give it once", a.short(db.String()))
			}
			if prev.Name() == db.Name() {
				return nil, nil, fmt.Errorf("the databases %s and %s would both be backed up as %s. Give one of them another name with --name NAME before its %s",
					a.short(prev.String()), a.short(db.String()), db.Name(), db.Flag())
			}
			why := "a file cannot also be a folder"
			if !seal.Clash(prev.Name(), db.Name()) {
				why = "they differ only by case, and with --plain-paths the repo would keep them as one file on macOS and Windows"
			}
			return nil, nil, fmt.Errorf("the databases %s and %s would be backed up as %s and %s, which clash because %s. Give one of them another name with --name NAME before its %s",
				a.short(prev.String()), a.short(db.String()), prev.Name(), db.Name(), why, db.Flag())
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key, err := seal.CopyKey(a.CacheDir, r.Root)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp("", "salt-db-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	extra = make([]seal.Extra, len(dbs))
	for i, db := range dbs {
		dst := filepath.Join(tmp, strconv.Itoa(i))
		meta, err := db.Copy(ctx, source.CopyOptions{Dst: dst, Key: key})
		if err != nil {
			cleanup()
			switch {
			case ctx.Err() != nil || errors.Is(err, context.Canceled):
				return nil, nil, fmt.Errorf("seal %w: the database copies were removed and nothing was sealed", ErrInterrupted)
			case errors.Is(err, fs.ErrNotExist):
				return nil, nil, fmt.Errorf("the database %s does not exist; check the path given to %s", a.short(db.String()), db.Flag())
			}
			return nil, nil, fmt.Errorf("copying the database %s: %w", a.short(db.String()), err)
		}
		extra[i] = seal.Extra{Rel: db.Name(), Path: dst, Mode: meta.Mode, ModTime: meta.ModTime}
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
