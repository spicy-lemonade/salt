package app

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// signStore keeps this machine's signing keys, one 0600 file per recipient.
// They are kept apart from the decryption keys, in a plain file rather than
// the keychain, so a scheduled seal never needs the keychain.
func (a *App) signStore() keys.FileStore { return keys.FileStore{Dir: a.SignDir} }

// errNoSigningKey means this machine has no key to sign backups to a repo.
var errNoSigningKey = errors.New("no signing key")

// signingKey returns this machine's key for signing backups to r: the one
// saved for the first of r's recipients that has one.
func (a *App) signingKey(r *repo.Repo) (ed25519.PrivateKey, error) {
	for _, rcpt := range r.RecipientStrings {
		s, err := a.signStore().Get(rcpt)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return s.SigningKey()
	}
	return nil, errNoSigningKey
}

// saveSigningKey derives the signing key from id and saves it.
func (a *App) saveSigningKey(id *age.X25519Identity) error {
	k := keys.SigningKey(id)
	if err := a.signStore().Set(id.Recipient().String(), keys.SigningSecret(k)); err != nil {
		return fmt.Errorf("saving the signing key: %w", err)
	}
	return nil
}

// ensureSigningKey makes sure this machine can sign backups to r. The key is
// derived from r's decryption key, taken from the keychain or by asking for
// the recovery phrase or passphrase.
func (a *App) ensureSigningKey(r *repo.Repo) error {
	if _, err := a.signingKey(r); !errors.Is(err, errNoSigningKey) {
		return err
	}
	ids, err := a.identities(r)
	if err != nil {
		return fmt.Errorf("salt needs your key once to set up signing on this machine: %w", err)
	}
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			if err := a.saveSigningKey(x); err != nil {
				return err
			}
		}
	}
	a.UI.Printf("✓ This machine can now sign backups to %s.\n", a.short(r.Root))
	return nil
}

// explainUnsigned adds what to do to an error from an index that is not
// signed by the person's key, or by one approved for the repo at root.
func explainUnsigned(err error, root string) error {
	if unapproved := (*seal.UnapprovedError)(nil); errors.As(err, &unapproved) {
		return fmt.Errorf("%w. If you added that key to the repo, run `salt trust %q`. "+
			"If you didn't, someone who can push may have copied in a backup from another repo, so check the repo's history before passing --allow-unsigned", err, root)
	}
	if !errors.Is(err, seal.ErrNotSigned) {
		return err
	}
	return fmt.Errorf("%w. Someone who can push to the backup repo may have replaced it to plant files. "+
		"If it was sealed by a salt that did not sign backups yet, or from a machine with a different key, check the repo's history, then pass --allow-unsigned", err)
}

// unsignedWarning is printed when --allow-unsigned let an unsigned index
// through. unapproved is the key that signed it, if it is one of the
// person's keys but not one approved for the repo.
func unsignedWarning(unapproved string) string {
	if unapproved != "" {
		return fmt.Sprintf("salt: ! the backup's index is signed by %s, a key not approved for this repo on this machine, "+
			"so salt cannot tell whether someone who can push copied it in from another repo\n", unapproved)
	}
	return "salt: ! the backup's index is not signed by your key, so salt cannot tell whether someone who can push to the repo planted files in it\n"
}
