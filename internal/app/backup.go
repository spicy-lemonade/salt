package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

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
	unlock, err := a.lockRepo(r.Root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.requireGitRepo(r.Root); err != nil {
		return err
	}
	if a.Git.Remote(r.Root) == "" {
		return fmt.Errorf("%s has no remote named origin to push the backup to. Add one with `git remote add origin URL`", a.short(r.Root))
	}
	// A commit with no branch checked out would be left behind.
	if _, err := a.Git.Branch(r.Root); err != nil {
		return fmt.Errorf("%s: %w", a.short(r.Root), err)
	}
	found, err := preset.Env{Getenv: a.Getenv, Home: a.Home}.Gather(o.Presets, r.Root, a.short)
	if err != nil {
		return err
	}
	// Checked before any database is copied, with advice that fits presets:
	// the paths come from them, not from the person.
	if rel, other, ok := seal.FirstClash(found.Paths(), !r.Format.EncryptPaths); ok {
		return fmt.Errorf("%w: the presets would back up %s and %s, which cannot both be in one backup. This is a problem in the presets, so please report it", seal.ErrDuplicatePath, other, rel)
	}
	for _, l := range found.LeftOut {
		advice := ""
		if l.Secret {
			advice = ". Keep secrets in environment variables so the file can be backed up"
		}
		a.UI.Printf("salt: left %s out of the backup because %s%s\n", a.short(l.Path), l.Why, advice)
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
	if err := a.Git.Commit(ctx, r.Root, backupMessage); err != nil {
		return fmt.Errorf("committing the backup: %w", err)
	}
	_, err = a.Git.Prune(r.Root, o.KeepDays)
	if err := a.pruneCleanup(err); err != nil {
		return fmt.Errorf("the backup was committed, but dropping old backups failed, so it was not pushed: %w", err)
	}
	if err := a.push(ctx, r.Root); err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return fmt.Errorf("backup %w: the backup was committed but not pushed", ErrInterrupted)
		}
		return fmt.Errorf("the backup was committed but not pushed: %w", err)
	}
	return nil
}

// maxKnownPushes caps how many commits salt remembers trying to push.
const maxKnownPushes = 100

// push pushes the backup to origin. Salt remembers, in its cache folder, the
// commit it last pushed and every one it has tried to push since, before
// each push. A push that reached origin, but whose answer was lost when the
// connection dropped, then still counts as this machine's on the next run,
// even after prune rewrote the branch.
func (a *App) push(ctx context.Context, root string) error {
	p, err := seal.RepoFile(a.CacheDir, root, "pushed-", ".json")
	if err != nil {
		return err
	}
	var known []string
	if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &known) != nil {
		known = nil // a damaged record only means the remote-tracking branch is used
	}
	head, err := a.Git.Head(root)
	if err != nil {
		return err
	}
	tried := known
	if !slices.Contains(tried, head) {
		tried = append(tried, head)
	}
	if err := writeKnown(p, tried[max(0, len(tried)-maxKnownPushes):]); err != nil {
		return err
	}
	if err := a.Git.Push(ctx, root, known); err != nil {
		return err
	}
	// Losing this only keeps older commits known until the next push.
	writeKnown(p, []string{head})
	return nil
}

func writeKnown(p string, known []string) error {
	b, err := json.Marshal(known)
	if err != nil {
		return err
	}
	return seal.WritePrivate(p, b)
}
