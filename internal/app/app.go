// Package app implements salt's commands on top of the core packages. It talks
// to the person only through UI and reaches git only through injected
// functions, so unit tests start no processes.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
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
	Now      func() time.Time
	Version  string
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

// Seal encrypts src into the salt repository at repoRoot.
func (a *App) Seal(src, repoRoot string, prune bool) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	if err := a.checkTrusted(r); err != nil {
		return err
	}
	res, err := seal.Seal(src, r, seal.Options{CacheDir: a.CacheDir, Prune: prune})
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
	if requireGitRepo(r.Root) != nil {
		return nil // not a git repo, so nothing is pushed
	}
	return a.checkStorage(r.Root, "salt: the backup was sealed, but")
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
	return p, hook.Install(p)
}

func requireGitRepo(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return fmt.Errorf("%s is not a git repository; clone or create your backup repo first", root)
	}
	return nil
}
