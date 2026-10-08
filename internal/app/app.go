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
	"github.com/spicy-lemonade/salt/internal/guard"
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
	// Branch returns the full name of the checked-out branch, or
	// gitx.ErrDetached.
	Branch(repoRoot string) (string, error)
	// Stage stages every change, removed files included.
	Stage(repoRoot string) error
	// Commit commits what is staged, and nothing when nothing is.
	Commit(ctx context.Context, repoRoot, msg string) error
	// Head returns the commit checked out.
	Head(repoRoot string) (string, error)
	// Lease reads where origin's branch is and returns it for Push, if it is
	// one of known or nothing there is lost (see gitx.Lease).
	Lease(ctx context.Context, repoRoot string, known []string) (string, error)
	// Push pushes the checked-out branch to origin if origin's branch is
	// still at lease (see gitx.Push).
	Push(ctx context.Context, repoRoot, lease string) error
	// PushSize returns about how many bytes Push would send to origin, whose
	// branch is at lease (see gitx.PushSize).
	PushSize(ctx context.Context, repoRoot, lease string) (int64, error)
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
func (RealGit) Branch(root string) (string, error) { return gitx.CurrentBranch(root) }
func (RealGit) Stage(root string) error            { return gitx.StageAll(root) }
func (RealGit) Commit(ctx context.Context, root, msg string) error {
	return gitx.CommitStaged(ctx, root, msg)
}
func (RealGit) Head(root string) (string, error) { return gitx.Head(root) }
func (RealGit) Lease(ctx context.Context, root string, known []string) (string, error) {
	return gitx.Lease(ctx, root, known)
}
func (RealGit) Push(ctx context.Context, root, lease string) error {
	return gitx.Push(ctx, root, lease)
}
func (RealGit) PushSize(ctx context.Context, root, lease string) (int64, error) {
	return gitx.PushSize(ctx, root, lease)
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
	// Live is true when Databases were found on this machine, as a preset
	// finds them, rather than given by the person. A database deleted
	// before it is copied is then left out instead of failing the seal.
	Live bool
}

// Seal encrypts o.Src, and safe copies of o.Databases, into the salt repository
// at o.Repo. An empty o.Src is refused: sealing nothing would remove every
// file sealed before, as when a backup script's variable is unset.
func (a *App) Seal(o SealOptions) error {
	if o.Src == "" {
		return errors.New("seal: SRC is empty. Give the folder to seal")
	}
	r, signer, err := a.openToSeal(o.Repo)
	if err != nil {
		return err
	}
	unlock, err := a.lockRepo(r.Root)
	if err != nil {
		return err
	}
	defer unlock()
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

// lockRepo stops two salts changing the repo at root at once, such as a
// backup still running, on a slow push, when the next one starts: each
// would delete the objects the other had just written. The lock is the
// state file lock (see stateFile).
func (a *App) lockRepo(root string) (unlock func(), err error) {
	p, err := a.stateFile(root, "lock", "")
	if err != nil {
		return nil, err
	}
	unlock, err = guard.Lock(p)
	if errors.Is(err, guard.ErrLocked) {
		return nil, fmt.Errorf("%s: %w", a.short(root), err)
	}
	return unlock, err
}

// stateFile returns where salt keeps what it must remember about the repo
// at root on this machine, such as its lock: name+ext in a salt folder in
// the repo's .git folder. That is never committed, and is the same whatever
// the environment, so a cron job and a shell agree on it. A repo with no
// .git folder, which salt seal accepts, or whose .git is a file, as in a
// git worktree, keeps it in the cache folder instead, named by the repo's
// real path.
func (a *App) stateFile(root, name, ext string) (string, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(filepath.Join(real, ".git")); err == nil && fi.IsDir() {
		return filepath.Join(real, ".git", "salt", name+ext), nil
	}
	return seal.RepoFile(a.CacheDir, real, name+"-", ext)
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
// The caller has already opened the repo as r, so o.Repo is ignored. A live
// database deleted before it was copied is listed in the result's Gone, as
// a live file deleted before it was read is.
func (a *App) seal(r *repo.Repo, signer ed25519.PrivateKey, o SealOptions) (*seal.Result, error) {
	extra, gone, cleanup, err := a.copyDatabases(o.Context, r, o.Databases, o.Live)
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
	res.Gone = append(res.Gone, gone...)
	return res, nil
}

// copyDatabases makes a safe copy of each live database in a new private
// temporary folder, and returns them as files to seal. cleanup removes the
// folder. With skipGone, a database that no longer exists is left out, and
// its name listed in gone.
func (a *App) copyDatabases(ctx context.Context, r *repo.Repo, dbs []source.Database, skipGone bool) (extra []seal.Extra, gone []string, cleanup func(), err error) {
	if len(dbs) == 0 {
		return nil, nil, func() {}, nil
	}
	// Two databases whose names clash are refused before any copy is made,
	// since a copy can take a long time. A clash with a source file is only
	// known once seal reads the source.
	names := make([]string, len(dbs))
	for i, db := range dbs {
		names[i] = db.Name()
	}
	if i, j, ok := seal.FirstClash(names, !r.Format.EncryptPaths); ok {
		prev, db := dbs[j], dbs[i]
		if prev.String() == db.String() && prev.Name() == db.Name() {
			return nil, nil, nil, fmt.Errorf("the database %s is given twice. Give it once", a.short(db.String()))
		}
		if prev.Name() == db.Name() {
			return nil, nil, nil, fmt.Errorf("the databases %s and %s would both be backed up as %s. Give one of them another name with --name NAME before its %s",
				a.short(prev.String()), a.short(db.String()), db.Name(), db.Flag())
		}
		why := "a file cannot also be a folder"
		if !seal.Clash(prev.Name(), db.Name()) {
			why = "they differ only by case, and with --plain-paths the repo would keep them as one file on macOS and Windows"
		}
		return nil, nil, nil, fmt.Errorf("the databases %s and %s would be backed up as %s and %s, which clash because %s. Give one of them another name with --name NAME before its %s",
			a.short(prev.String()), a.short(db.String()), prev.Name(), db.Name(), why, db.Flag())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key, err := seal.CopyKey(a.CacheDir, r.Root)
	if err != nil {
		return nil, nil, nil, err
	}
	tmp, err := os.MkdirTemp("", "salt-db-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	for i, db := range dbs {
		dst := filepath.Join(tmp, strconv.Itoa(i))
		meta, err := db.Copy(ctx, source.CopyOptions{Dst: dst, Key: key})
		if skipGone && errors.Is(err, fs.ErrNotExist) && ctx.Err() == nil {
			gone = append(gone, db.Name())
			continue // cleanup removes any part of a copy it made
		}
		if err != nil {
			cleanup()
			switch {
			case ctx.Err() != nil || errors.Is(err, context.Canceled):
				return nil, nil, nil, fmt.Errorf("seal %w: the database copies were removed and nothing was sealed", ErrInterrupted)
			case errors.Is(err, fs.ErrNotExist):
				return nil, nil, nil, fmt.Errorf("the database %s does not exist; check the path given to %s", a.short(db.String()), db.Flag())
			}
			return nil, nil, nil, fmt.Errorf("copying the database %s: %w", a.short(db.String()), err)
		}
		extra = append(extra, seal.Extra{Rel: db.Name(), Path: dst, Mode: meta.Mode, ModTime: meta.ModTime})
	}
	return extra, gone, cleanup, nil
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
