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

// approval returns the keys and settings this machine approved for r, and
// how r differs from them. It returns trust.ErrNotApproved if this machine
// never approved r.
func (a *App) approval(r *repo.Repo) (trust.Pin, []string, error) {
	pin, err := a.trustStore().Load(r.Root)
	if errors.Is(err, trust.ErrNotApproved) {
		return trust.Pin{}, nil, err
	}
	if err != nil {
		return trust.Pin{}, nil, fmt.Errorf("cannot read the keys approved for %s on this machine (%w)", a.short(r.Root), err)
	}
	return pin, trust.Diff(pin, trust.For(r)), nil
}

// checkTrusted refuses to seal unless the repo's keys and settings match the
// ones approved on this machine.
func (a *App) checkTrusted(r *repo.Repo) error {
	pin, d, err := a.approval(r)
	if errors.Is(err, trust.ErrNotApproved) {
		return fmt.Errorf("%w: this machine has not approved the keys in %s yet. Check them and run `salt trust %q`",
			ErrNotTrusted, a.short(r.Root), r.Root)
	}
	if err != nil {
		return fmt.Errorf("%w. Run `salt trust %q` to replace it", err, r.Root)
	}
	if len(d) > 0 {
		return fmt.Errorf("%w: the keys or settings in %s changed since you approved them:\n  %s\n"+
			"If you made this change, run `salt trust %q`. If you didn't, someone else changed your backup repo. Don't back up until you've checked it",
			ErrNotTrusted, a.short(r.Root), strings.Join(d, "\n  "), r.Root)
	}
	// An approval saved by a salt that did not record how the key is
	// recovered records it now, as the rest was recorded when approved.
	// Losing this save only means trying again next time.
	if pin.Recovery == "" && r.Format.Recovery != "" {
		pin.Recovery = r.Format.Recovery
		a.trustStore().Save(r.Root, pin)
	}
	return nil
}

// approvedSigners returns the keys this machine approved for r, the only
// ones restore and verify accept a signature from. Otherwise someone who can
// push could add another of the person's keys to the repo, with a backup
// that key signed for a different repo, and it would restore as genuine. It
// warns if r's keys or settings changed since they were approved. With no
// approval it warns and returns nil, and any key that opens the backup may
// have signed it. An approval that can't be read stops the command, unless
// allowUnsigned, which then warns and returns nil in the same way.
func (a *App) approvedSigners(r *repo.Repo, allowUnsigned bool) ([]string, error) {
	pin, d, err := a.approval(r)
	if errors.Is(err, trust.ErrNotApproved) {
		a.UI.Printf("salt: ! this machine has not approved the keys in %s, so any of your keys may have signed it. Run `salt trust %q` to approve them\n",
			a.short(r.Root), r.Root)
		return nil, nil
	}
	if err != nil && allowUnsigned {
		a.UI.Printf("salt: ! %v, so any of your keys may have signed the backup\n", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w. Run `salt trust %q` to replace it, or pass --allow-unsigned", err, r.Root)
	}
	if len(d) > 0 {
		a.UI.Printf("salt: ! the keys or settings in %s changed since you approved them:\n  %s\n"+
			"Only a backup signed by an approved key is accepted. If you made this change, run `salt trust %q`\n",
			a.short(r.Root), strings.Join(d, "\n  "), r.Root)
	}
	return pin.Recipients, nil
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
		switch _, err := a.storedIdentity(rcpt); {
		case errors.Is(err, keys.ErrNotFound):
			mark = "  ⚠ NOT on this machine"
			unknown++
		case errors.Is(err, errDamagedKey):
			mark = "  ⚠ " + errDamagedKey.Error()
			unknown++
		case err != nil:
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
	// An approval that can't be read is replaced, since this is how to fix it.
	switch _, d, err := a.approval(r); {
	case errors.Is(err, trust.ErrNotApproved):
	case err != nil:
		a.UI.Printf("\n⚠ %v. Approving replaces it.\n", err)
	case len(d) > 0:
		a.UI.Printf("\nChanged since you last approved:\n  %s\n", strings.Join(d, "\n  "))
	default:
		a.UI.Printf("\nNothing has changed since you last approved.\n")
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
