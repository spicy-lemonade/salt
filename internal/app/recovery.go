package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// RecoveryTest asks for the recovery phrase or passphrase and confirms it
// opens this backup, without decrypting anything.
func (a *App) RecoveryTest(repoRoot string) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	if !a.UI.Interactive() {
		return ErrNotInteractive
	}
	if _, _, err := a.promptIdentity(r); err != nil {
		return err
	}
	a.UI.Printf("✓ Your %s opens this backup.\n", recoveryNoun(r))
	return nil
}

// RecoveryShow displays the recovery phrase saved on this machine.
func (a *App) RecoveryShow(repoRoot string) error {
	r, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	if r.Format.Recovery != repo.RecoveryPhrase {
		return errors.New("this backup uses a passphrase, not a recovery phrase; salt never stores your passphrase, so it cannot show it")
	}
	if !a.UI.Interactive() {
		return ErrNotInteractive
	}
	for _, rcpt := range r.RecipientStrings {
		s, err := a.Store.Get(rcpt)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		words, err := s.Phrase()
		if err != nil {
			continue
		}
		ans, err := a.UI.ReadLine("Anybody with these words can decrypt and read your backups. Show them now? [y/N] ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(ans), "y") {
			return nil
		}
		a.UI.Printf("\n%s\n", formatPhrase(words))
		if _, err := a.UI.ReadLine("Press Enter to hide them."); err != nil {
			return err
		}
		a.UI.Clear()
		return nil
	}
	return fmt.Errorf("no recovery phrase for this backup is saved on this machine (%s)", a.StoreName)
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	Repo  string
	To    string
	Paths []string
	Force bool
	// Context stops the restore when cancelled (see seal.RestoreOptions).
	Context context.Context
}

// Restore decrypts a backup into o.To.
func (a *App) Restore(o RestoreOptions) error {
	r, err := repo.Open(o.Repo)
	if err != nil {
		return err
	}
	ids, err := a.identities(r)
	if err != nil {
		return err
	}
	a.warnLeftoverRestores(o.To)
	res, err := seal.Restore(r.Root, ids, o.To, seal.RestoreOptions{
		Paths: o.Paths, Force: o.Force, Context: o.Context, Track: a.trackRestore, Show: a.short,
	})
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("restore %w: the partly restored files were removed and %s was not changed", ErrInterrupted, a.short(o.To))
	}
	if err != nil {
		return err
	}
	a.UI.Printf("✓ Restored %d files and %d symlinks to %s\n", res.Files, res.Symlinks, a.short(o.To))
	if res.MovedAside != "" {
		a.UI.Printf("  The previous contents were moved to %s\n", a.short(res.MovedAside))
	}
	return nil
}

// identities returns the keys that open r: from the keychain, or by asking
// for the recovery phrase or passphrase (then offering to save the key).
func (a *App) identities(r *repo.Repo) ([]age.Identity, error) {
	var ids []age.Identity
	for _, rcpt := range r.RecipientStrings {
		s, err := a.Store.Get(rcpt)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		id, err := s.Identity()
		if err != nil {
			return nil, err
		}
		if id.Recipient().String() == rcpt {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		return ids, nil
	}
	if !a.UI.Interactive() {
		return nil, fmt.Errorf("no key for this backup is saved on this machine; run this in a terminal to enter your %s", recoveryNoun(r))
	}
	a.UI.Printf("No key for this backup is saved on this machine.\n")
	id, secret, err := a.promptIdentity(r)
	if err != nil {
		return nil, err
	}
	ans, err := a.UI.ReadLine(fmt.Sprintf("Save the key in the %s so you are not asked again? [Y/n] ", a.StoreName))
	if err != nil {
		return nil, err
	}
	if yes := strings.TrimSpace(strings.ToLower(ans)); yes == "" || yes == "y" || yes == "yes" {
		if err := a.Store.Set(id.Recipient().String(), secret); err != nil {
			return nil, fmt.Errorf("saving your key: %w", err)
		}
		a.UI.Printf("✓ Key saved.\n")
	}
	return []age.Identity{id}, nil
}

// promptIdentity asks for the recovery phrase or passphrase and checks the
// resulting key belongs to this backup.
func (a *App) promptIdentity(r *repo.Repo) (*age.X25519Identity, keys.Secret, error) {
	var (
		id     *age.X25519Identity
		secret keys.Secret
	)
	switch r.Format.Recovery {
	case repo.RecoveryPhrase:
		entropy, err := a.readPhrase()
		if err != nil {
			return nil, secret, err
		}
		if id, err = keys.IdentityFromEntropy(entropy); err != nil {
			return nil, secret, err
		}
		secret = keys.PhraseSecret(entropy)
	case repo.RecoveryPassphrase:
		data, err := os.ReadFile(filepath.Join(r.Root, repo.KeyFile))
		if err != nil {
			return nil, secret, fmt.Errorf("reading %s: %w", repo.KeyFile, err)
		}
		p, err := a.UI.ReadSecret("Passphrase: ")
		if err != nil {
			return nil, secret, err
		}
		if id, err = keys.UnwrapIdentity(data, p); err != nil {
			return nil, secret, err
		}
		secret = keys.IdentitySecret(id)
	default:
		return nil, secret, fmt.Errorf("unknown recovery method %q in %s", r.Format.Recovery, repo.FormatFile)
	}
	if !slices.Contains(r.RecipientStrings, id.Recipient().String()) {
		return nil, secret, fmt.Errorf("that is a valid %s, but not the one for this backup", recoveryNoun(r))
	}
	return id, secret, nil
}

func recoveryNoun(r *repo.Repo) string {
	if r.Format.Recovery == repo.RecoveryPassphrase {
		return "passphrase"
	}
	return "recovery phrase"
}
