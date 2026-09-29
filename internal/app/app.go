// Package app implements salt's commands on top of the core packages. It talks
// to the person only through UI and reaches git only through injected
// functions, so unit tests start no processes.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spicy-lemonade/salt/internal/check"
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
	// HookPath resolves a repo's pre-commit hook path (gitx.HookPath).
	HookPath func(repoRoot string) (string, error)
	// Staged checks a repo's staged files (check.Staged).
	Staged func(repoRoot string) ([]check.Violation, error)
}

// Seal encrypts src into the salt repository at repoRoot.
func (a *App) Seal(src, repoRoot string, prune bool) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
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
	return nil
}

// ErrCheckFailed is returned when staged files are not encrypted.
var ErrCheckFailed = errors.New("plaintext staged")

// Check refuses plaintext staged in the repository at repoRoot.
func (a *App) Check(repoRoot string) error {
	vs, err := a.Staged(repoRoot)
	if err != nil {
		return fmt.Errorf("salt check could not inspect the commit, refusing it: %w", err)
	}
	if len(vs) > 0 {
		a.UI.Printf("%s", check.Report(vs))
		return ErrCheckFailed
	}
	return nil
}

// InstallHook installs the pre-commit hook in the repository at repoRoot.
func (a *App) InstallHook(repoRoot string) (string, error) {
	p, err := a.HookPath(repoRoot)
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
