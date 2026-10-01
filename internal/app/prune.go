package app

import (
	"errors"

	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// Prune keeps only the backups from the keepDays most recent days on which
// anything in the repo changed, and drops older ones from the checked-out
// branch. It refuses a repo this machine has not approved: if someone else
// changed its keys, the history that shows it must not be rewritten away.
func (a *App) Prune(repoRoot string, keepDays int) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	if err := a.requireGitRepo(r.Root); err != nil {
		return err
	}
	if err := a.checkTrusted(r); err != nil {
		return err
	}
	res, err := a.Git.Prune(r.Root, keepDays)
	if res == nil {
		return err
	}
	if res.Dropped == 0 {
		a.UI.Printf("salt: nothing to prune: all %d backup(s) are from the last %d day(s) with a change\n", res.Kept, keepDays)
	} else {
		a.UI.Printf("salt: kept %d backup(s) from the last %d day(s) with a change and dropped %d older one(s)\n",
			res.Kept, res.Days, res.Dropped)
		a.UI.Printf("salt: history was rewritten, so push with `git push --force-with-lease`\n")
	}
	// The branch has already moved, so a failed clean-up leaves every kept
	// backup complete; it only leaves the dropped ones on this disk until the
	// next prune. It is a warning, so a backup script still goes on to push.
	if errors.Is(err, prune.ErrCleanup) {
		a.UI.Printf("salt: warning: %v; they take up space on this machine until the next prune removes them\n", err)
		return nil
	}
	return err
}
