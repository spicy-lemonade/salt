package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/trust"
)

func (a *App) trustStore() trust.Store { return trust.Store{Dir: a.TrustDir} }

// ErrNotTrusted is returned when a repo's keys or settings have not been
// approved on this machine, or have changed since.
var ErrNotTrusted = errors.New("backup repo not approved")

// checkTrusted refuses to seal unless the repo's keys and settings match the
// ones approved on this machine.
func (a *App) checkTrusted(r *repo.Repo) error {
	approved, err := a.trustStore().Load(r.Root)
	if errors.Is(err, trust.ErrNotApproved) {
		return fmt.Errorf("%w: this machine has not approved the keys in %s yet. Check them and run `salt trust %q`",
			ErrNotTrusted, a.short(r.Root), r.Root)
	}
	if err != nil {
		return err
	}
	if d := trust.Diff(approved, trust.For(r)); len(d) > 0 {
		return fmt.Errorf("%w: the keys or settings in %s changed since you approved them:\n  %s\n"+
			"If you made this change, run `salt trust %q`. If you didn't, someone else changed your backup repo. Don't back up until you've checked it",
			ErrNotTrusted, a.short(r.Root), strings.Join(d, "\n  "), r.Root)
	}
	return nil
}

// approvedSigners returns the keys this machine approved for r, the only
// ones restore and verify accept a signature from. Otherwise someone who can
// push could add another of the person's keys to the repo, with a backup
// that key signed for a different repo, and it would restore as genuine. It
// warns if r's keys or settings changed since they were approved. With no
// approval it returns nil, and any key that opens the backup may have signed
// it. An approval that can't be read stops the command, unless allowUnsigned,
// which skips the signature check anyway.
func (a *App) approvedSigners(r *repo.Repo, allowUnsigned bool) ([]string, error) {
	approved, err := a.trustStore().Load(r.Root)
	if errors.Is(err, trust.ErrNotApproved) {
		return nil, nil
	}
	if err != nil && allowUnsigned {
		a.UI.Printf("salt: ! cannot read the keys approved for %s on this machine (%v)\n", a.short(r.Root), err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the keys approved for %s on this machine (%w). Delete that file and run `salt trust %q`, or pass --allow-unsigned",
			a.short(r.Root), err, r.Root)
	}
	if d := trust.Diff(approved, trust.For(r)); len(d) > 0 {
		a.UI.Printf("salt: ! the keys or settings in %s changed since you approved them:\n  %s\n"+
			"Only a backup signed by an approved key is accepted. If you made this change, run `salt trust %q`\n",
			a.short(r.Root), strings.Join(d, "\n  "), r.Root)
	}
	return approved.Recipients, nil
}

// Trust shows a repo's keys and settings and approves them for backups from
// this machine. yes skips the question, for scripts.
func (a *App) Trust(repoRoot string, yes bool) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	pin := trust.For(r)
	a.UI.Printf("Backups to %s will be encrypted to these keys:\n", a.short(r.Root))
	unknown := 0
	for _, rcpt := range pin.Recipients {
		mark := "  (your key on this machine)"
		if _, err := a.Store.Get(rcpt); errors.Is(err, keys.ErrNotFound) {
			mark = "  ⚠ NOT on this machine"
			unknown++
		} else if err != nil {
			return err
		}
		a.UI.Printf("  %s%s\n", rcpt, mark)
	}
	a.UI.Printf("File names are %s.\n", map[bool]string{true: "hidden", false: "visible"}[pin.EncryptPaths])
	if unknown > 0 {
		a.UI.Printf("\n⚠ %d key(s) are not stored on this machine. Whoever holds them can read every backup.\n"+
			"  Only approve if you know whose they are, for example your own recovery phrase or another laptop of yours.\n", unknown)
	}
	if !pin.EncryptPaths {
		a.UI.Printf("\n⚠ File names are visible. Anyone who can see the repo can read your folder and file names.\n" +
			"  The contents are still encrypted.\n")
	}
	if approved, err := a.trustStore().Load(r.Root); err == nil {
		if d := trust.Diff(approved, pin); len(d) > 0 {
			a.UI.Printf("\nChanged since you last approved:\n  %s\n", strings.Join(d, "\n  "))
		} else {
			a.UI.Printf("\nNothing has changed since you last approved.\n")
		}
	} else if !errors.Is(err, trust.ErrNotApproved) {
		return err
	}
	if !yes {
		if !a.UI.Interactive() {
			return fmt.Errorf("%w; or pass --yes", ErrNotInteractive)
		}
		ans, err := a.UI.ReadLine("\nAnybody holding one of these keys can read your backups. Approve them? [y/N] ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(ans), "y") {
			a.UI.Printf("Not approved. Nothing was changed.\n")
			return nil
		}
	}
	if err := a.ensureSigningKey(r); err != nil {
		return err
	}
	if err := a.trustStore().Save(r.Root, pin); err != nil {
		return err
	}
	a.UI.Printf("✓ Approved. Backups from this machine will use these keys.\n")
	return nil
}
