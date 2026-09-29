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
		return fmt.Errorf("%w: this machine has not approved the keys in %s yet. Check them and run `salt trust %s`",
			ErrNotTrusted, r.Root, r.Root)
	}
	if err != nil {
		return err
	}
	if d := trust.Diff(approved, trust.For(r)); len(d) > 0 {
		return fmt.Errorf("%w: the keys or settings in %s changed since you approved them:\n  %s\n"+
			"If you made this change, run `salt trust %s`. If you didn't, someone else changed your backup repo. Don't back up until you've checked it",
			ErrNotTrusted, r.Root, strings.Join(d, "\n  "), r.Root)
	}
	return nil
}

// Trust shows a repo's keys and settings and approves them for backups from
// this machine. yes skips the question, for scripts.
func (a *App) Trust(repoRoot string, yes bool) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	pin := trust.For(r)
	a.UI.Printf("Backups to %s will be encrypted to these keys:\n", r.Root)
	for _, rcpt := range pin.Recipients {
		mark := ""
		if _, err := a.Store.Get(rcpt); err == nil {
			mark = "  (your key on this machine)"
		} else if !errors.Is(err, keys.ErrNotFound) {
			return err
		}
		a.UI.Printf("  %s%s\n", rcpt, mark)
	}
	a.UI.Printf("File names are %s.\n", map[bool]string{true: "hidden", false: "visible"}[pin.EncryptPaths])
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
	if err := a.trustStore().Save(r.Root, pin); err != nil {
		return err
	}
	a.UI.Printf("✓ Approved. Backups from this machine will use these keys.\n")
	return nil
}
