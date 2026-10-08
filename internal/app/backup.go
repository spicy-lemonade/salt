package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

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
	paths := found.Paths()
	if i, j, ok := seal.FirstClash(paths, !r.Format.EncryptPaths); ok {
		return fmt.Errorf("%w: the presets would back up %s and %s, which cannot both be in one backup. This is a problem in the presets, so please report it", seal.ErrDuplicatePath, paths[j], paths[i])
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
	for _, n := range found.Numbers {
		a.UI.Printf("salt: warning: the setting %s in %s holds a number, which salt does not take for a secret, so the file is backed up. If it is a secret, keep it in an environment variable\n", n.Key, a.short(n.Path))
	}
	// Read before sealing, which replaces them with this backup's.
	last := seal.CachedPaths(a.CacheDir, r.Root)
	// A signal stops the backup before the next step that changes the repo,
	// saying how far it got. Prune most of all must not start after one.
	stopped := func(done string) error {
		if ctx.Err() != nil {
			return fmt.Errorf("backup %w: %s", ErrInterrupted, done)
		}
		return nil
	}
	if err := stopped("nothing was backed up"); err != nil {
		return err
	}
	so := SealOptions{Prune: true, Databases: found.Databases, Files: found.Files, Context: ctx, Live: true}
	sealed, err := a.seal(r, signer, so)
	if err != nil {
		return err
	}
	a.warnMissing(last, o.Presets, found, sealed.Gone)
	if err := found.CheckGone(sealed.Gone); err != nil {
		return fmt.Errorf("the backup was sealed but not committed: %w", err)
	}
	if err := a.Git.Stage(r.Root); err != nil {
		return fmt.Errorf("staging the backup: %w", err)
	}
	// The same check as the pre-commit hook, which salt never runs itself.
	if err := a.Check(r.Root); err != nil {
		return err
	}
	if err := stopped("the backup was sealed and staged but not committed"); err != nil {
		return err
	}
	if err := a.Git.Commit(ctx, r.Root, backupMessage); err != nil {
		if err := stopped("the backup was sealed and staged but not committed"); err != nil {
			return err
		}
		return fmt.Errorf("committing the backup: %w", err)
	}
	if err := stopped("the backup was committed, but old backups were not dropped and it was not pushed"); err != nil {
		return err
	}
	// Origin is checked before prune rewrites the branch: a commit pushed by
	// hand is held by the branch only until then.
	record, known, err := a.pushRecord(r.Root)
	if err != nil {
		return err
	}
	lease, err := a.Git.Lease(ctx, r.Root, known)
	if err != nil {
		if interrupted(ctx, err) {
			return fmt.Errorf("backup %w: the backup was committed, but old backups were not dropped and it was not pushed", ErrInterrupted)
		}
		return fmt.Errorf("the backup was committed, but salt could not check origin, so old backups were not dropped and it was not pushed: %w", err)
	}
	// Ctrl-C can reach git before salt cancels ctx, so an error from git
	// that says it was stopped counts too. Prune must not start after one.
	if err := a.warnPush(ctx, r.Root, lease, sealed.Written); interrupted(ctx, err) {
		return fmt.Errorf("backup %w: the backup was committed, but old backups were not dropped and it was not pushed", ErrInterrupted)
	}
	_, err = a.Git.Prune(r.Root, o.KeepDays)
	if err := a.pruneCleanup(err); err != nil {
		return fmt.Errorf("the backup was committed, but dropping old backups failed, so it was not pushed: %w", err)
	}
	if err := a.push(ctx, r.Root, record, known, lease); err != nil {
		if interrupted(ctx, err) {
			return fmt.Errorf("backup %w: the backup was committed but not pushed", ErrInterrupted)
		}
		return fmt.Errorf("the backup was committed but not pushed: %w", err)
	}
	return nil
}

// interrupted reports whether err came from Ctrl-C or SIGTERM, which may
// reach git before ctx is cancelled.
func interrupted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

// warnMissing names each place the last backup held, at the paths in last,
// that was not found this time and that holds nothing of its own in this
// backup, not counting a place inside it that is found. Such a place may
// be on a drive that is not mounted, be pointed to by a variable set in the
// person's shell but not for cron, or be a file a preset names that was
// deleted before it was sealed. gone lists the backup paths of what found held but was deleted before it
// was sealed. Its files are no longer backed up, so each place is named
// once, not each file, and one inside another missing place is not named.
// Paths no preset names, such as those an earlier salt seal sealed, are not
// named.
func (a *App) warnMissing(last []string, presets []*preset.Preset, found *preset.Found, gone []string) {
	// still records, for each place, whether it is backed up this time:
	// whether this backup holds a path whose own place it is, so a place
	// inside it that is found does not count, or it was found and was not
	// a file deleted before it was sealed.
	still := map[string]bool{}
	for _, rel := range found.Paths() {
		if place, ok := preset.PlaceOf(presets, rel); ok && !slices.Contains(gone, rel) {
			still[place] = true
		}
	}
	for _, rel := range last {
		place, ok := preset.PlaceOf(presets, rel)
		if _, seen := still[place]; !ok || seen {
			continue
		}
		still[place] = slices.Contains(found.Places, place) && !slices.Contains(gone, place)
	}
	in := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	var missing []string
	for place, ok := range still {
		if !ok {
			missing = append(missing, place)
		}
	}
	slices.Sort(missing)
	for _, place := range missing {
		if !slices.ContainsFunc(missing, func(o string) bool { return o != place && in(place, o) }) {
			a.UI.Printf("salt: %s was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n", place)
		}
	}
}

// Sizes above which salt backup warns before it pushes. GitHub refuses a
// push over 2 GB.
const (
	warnPushBytes int64 = 1 << 30
	warnFileBytes int64 = 500 << 20
)

// warnPush warns about each file that added more than warnFileBytes of new
// ciphertext to this backup, naming it, and about a push of more than
// warnPushBytes, which counts earlier backups not yet pushed too. The push
// is measured before prune, while the commit origin holds is sure to be in
// the local repo, so after failed pushes it can also count backups prune is
// about to drop. The push still goes ahead. Measuring the push is best
// effort: when git cannot measure it, as one older than 2.31 cannot, only
// the files are named. It returns the error from measuring, which the
// caller checks only for an interruption.
func (a *App) warnPush(ctx context.Context, root, lease string, written []seal.Written) error {
	for _, w := range written {
		if w.Bytes > warnFileBytes {
			a.UI.Printf("salt: warning: %s added %s of encrypted data to this backup. A large file that changes often makes the backup repo grow quickly, so back it up once a day at most\n", w.Path, sizeText(w.Bytes))
		}
	}
	n, err := a.Git.PushSize(ctx, root, lease)
	if err == nil && n > warnPushBytes {
		a.UI.Printf("salt: warning: this push sends up to about %s. GitHub refuses a push over 2 GB\n", sizeText(n))
	}
	return err
}

// sizeText writes n bytes as MiB, or GiB from 1 GiB.
func sizeText(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", (n+1<<19)>>20)
}

// maxKnownPushes caps how many commits salt remembers trying to push.
const maxKnownPushes = 100

// pushRecord returns where salt remembers the commit it last pushed from the
// repo at root and every one it has tried to push since (the state file
// pushed.json, see stateFile), and those commits. A push that reached origin, but whose answer was
// lost when the connection dropped, then still counts as this machine's on
// the next run, even after prune rewrote the branch. A damaged record is set
// aside, and only a commit the branch holds then counts.
func (a *App) pushRecord(root string) (path string, known []string, err error) {
	path, err = a.stateFile(root, "pushed", ".json")
	if err != nil {
		return "", nil, err
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &known) != nil {
		known = nil
	}
	return path, known, nil
}

// push pushes the backup to origin, leased to the commit Lease found there.
// It first adds the commit it pushes to known in the record at path, and
// once the push is done, the record holds only that commit.
func (a *App) push(ctx context.Context, root, path string, known []string, lease string) error {
	head, err := a.Git.Head(root)
	if err != nil {
		return err
	}
	tried := known
	if !slices.Contains(tried, head) {
		tried = append(tried, head)
	}
	if err := writeKnown(path, tried[max(0, len(tried)-maxKnownPushes):]); err != nil {
		return err
	}
	if err := a.Git.Push(ctx, root, lease); err != nil {
		return err
	}
	// Losing this only keeps older commits known until the next push.
	writeKnown(path, []string{head})
	return nil
}

func writeKnown(p string, known []string) error {
	b, _ := json.Marshal(known) // a list of strings always marshals
	return seal.WritePrivate(p, b)
}
