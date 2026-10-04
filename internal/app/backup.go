package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spicy-lemonade/salt/internal/preset"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// backupMessage is the message of every commit salt backup makes.
const backupMessage = "salt backup"

// BackupOptions configures Backup.
type BackupOptions struct {
	Repo    string
	Presets []*preset.Preset
	// KeepDays is how many days with a change to keep (see prune.Run).
	KeepDays int
	// Context stops the database copies and the push early. Nil means never.
	Context context.Context
}

// Backup gathers what o.Presets name on this machine and seals it into the
// salt repository at o.Repo, which then holds only that. It commits the
// backup when anything changed, drops old backups and pushes. It prints
// nothing when all goes well, so a scheduled job stays silent.
func (a *App) Backup(o BackupOptions) error {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	r, signer, err := a.openToSeal(o.Repo)
	if err != nil {
		return err
	}
	if err := a.requireGitRepo(r.Root); err != nil {
		return err
	}
	if a.Git.Remote(r.Root) == "" {
		return fmt.Errorf("%s has no remote named origin to push the backup to. Add one with `git remote add origin URL`", a.short(r.Root))
	}
	found, err := preset.Env{Getenv: a.Getenv, Home: a.Home}.Gather(o.Presets, a.short)
	if err != nil {
		return err
	}
	// The roots have their symlinks followed, so the repo's are too before
	// comparing them.
	repoReal, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return err
	}
	for _, root := range found.Roots {
		if err := seal.CheckDisjoint(root, repoReal, a.short); err != nil {
			return err
		}
	}
	for _, l := range found.LeftOut {
		a.UI.Printf("salt: left %s out of the backup because %s. Keep secrets in environment variables so the file can be backed up\n", a.short(l.Path), l.Why)
	}
	for _, s := range found.Skipped {
		a.UI.Printf("salt: skipped %s (not a file)\n", s)
	}
	so := SealOptions{Repo: o.Repo, Prune: true, Databases: found.Databases, Files: found.Files, Context: ctx}
	if _, err := a.seal(r, signer, so); err != nil {
		return err
	}
	if err := a.Git.Stage(r.Root); err != nil {
		return fmt.Errorf("staging the backup: %w", err)
	}
	// The same check as the pre-commit hook, which salt never runs itself.
	if err := a.Check(r.Root); err != nil {
		return err
	}
	if err := a.Git.Commit(r.Root, backupMessage); err != nil {
		return fmt.Errorf("committing the backup: %w", err)
	}
	_, err = a.Git.Prune(r.Root, o.KeepDays)
	if err := a.pruneCleanup(err); err != nil {
		return fmt.Errorf("the backup was committed, but dropping old backups failed, so it was not pushed: %w", err)
	}
	if err := a.Git.Push(ctx, r.Root); err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return fmt.Errorf("backup %w: the backup was committed but not pushed", ErrInterrupted)
		}
		return fmt.Errorf("the backup was committed but not pushed: %w", err)
	}
	return nil
}
