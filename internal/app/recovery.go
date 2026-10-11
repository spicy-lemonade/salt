package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
		return stopped(err, "recovery test", "")
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
	saved, note := a.savedKeys(r.RecipientStrings)
	for _, k := range saved {
		if errors.Is(k.err, keys.ErrNotFound) || errors.Is(k.err, errDamagedKey) {
			continue
		}
		if k.err != nil {
			return k.err
		}
		words, err := k.secret.Phrase()
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
	return fmt.Errorf("no recovery phrase for this backup is saved on this machine (%s)%s", a.StoreName, note)
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// Repo is the backup repo's folder, or its https or SSH URL, from which
	// only the latest backup is downloaded.
	Repo  string
	To    string
	Paths []string
	Force bool
	// AllowUnsigned restores even if the index is not signed by your key.
	AllowUnsigned bool
	// Context stops the restore when cancelled (see seal.RestoreOptions).
	Context context.Context
}

// Restore decrypts a backup into o.To.
func (a *App) Restore(o RestoreOptions) (err error) {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	// A question about the key stops when ctx does.
	a.UI.SetContext(ctx)
	r, downloaded, done, err := a.openRepo(ctx, o.Repo)
	if errors.Is(err, errDownloadStopped) {
		return fmt.Errorf("restore %w: %s was not changed", ErrInterrupted, a.short(o.To))
	}
	if err != nil {
		return err
	}
	defer func() { err = done(err) }()
	// A download is new to this machine, so it has no approval to check.
	var signedBy []string
	if !downloaded {
		if signedBy, err = a.approvedSigners(r, o.AllowUnsigned); err != nil {
			return err
		}
	}
	ids, err := a.identities(r)
	if err != nil {
		// Ctrl-C or SIGTERM while salt asks for the recovery phrase or
		// passphrase stops the question (see UI.SetContext and
		// Terminal.readHidden).
		if ctx.Err() != nil {
			err = ErrInterrupted
		}
		return stopped(err, "restore", fmt.Sprintf(": %s was not changed", a.short(o.To)))
	}
	a.warnLeftoverRestores(o.To)
	res, err := seal.Restore(r.Root, ids, o.To, seal.RestoreOptions{
		Paths: o.Paths, Force: o.Force, Context: o.Context, Track: a.trackRestore, Show: a.short,
		SignatureOptions: seal.SignatureOptions{AllowUnsigned: o.AllowUnsigned, SignedBy: signedBy},
	})
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("restore %w: the partly restored files were removed and %s was not changed", ErrInterrupted, a.short(o.To))
	}
	if err != nil {
		return explainUnsigned(err, r.Root)
	}
	if res.Unsigned {
		a.UI.Printf("%s", unsignedWarning(res.Unapproved))
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
	saved, note := a.savedKeys(r.RecipientStrings)
	for _, k := range saved {
		if errors.Is(k.err, keys.ErrNotFound) || errors.Is(k.err, errDamagedKey) {
			continue
		}
		if k.err != nil {
			return nil, k.err
		}
		ids = append(ids, k.id)
	}
	if len(ids) > 0 {
		return ids, nil
	}
	if !a.UI.Interactive() {
		return nil, fmt.Errorf("no key for this backup is saved on this machine%s; run this in a terminal to enter your %s", note, recoveryNoun(r))
	}
	a.UI.Printf("No key for this backup is saved on this machine%s.\n", note)
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

// errDamagedKey means the key saved on this machine for a public key cannot
// be read, or is the key for another one.
var errDamagedKey = errors.New("the key saved for it on this machine is damaged")

// storedIdentity returns the key saved on this machine for the public key
// rcpt. It returns keys.ErrNotFound when none is saved, and errDamagedKey
// unless the one saved really is rcpt's.
func (a *App) storedIdentity(rcpt string) (*age.X25519Identity, error) {
	_, id, err := a.storedKey(rcpt)
	return id, err
}

// storedKey is storedIdentity, with the secret the key was read from.
func (a *App) storedKey(rcpt string) (keys.Secret, *age.X25519Identity, error) {
	s, err := a.Store.Get(rcpt)
	if errors.Is(err, keys.ErrDamaged) {
		return keys.Secret{}, nil, errDamagedKey
	}
	if err != nil {
		return keys.Secret{}, nil, err
	}
	id, err := s.Identity()
	if err != nil || id.Recipient().String() != rcpt {
		return keys.Secret{}, nil, errDamagedKey
	}
	return s, id, nil
}

// savedKey is what this machine holds for one of a repo's public keys.
type savedKey struct {
	rcpt   string
	secret keys.Secret
	id     *age.X25519Identity
	// err is nil when id is the key saved for rcpt, and otherwise as
	// storedKey returns it.
	err error
}

// savedKeys returns what this machine holds for each of the public keys
// rcpts, in order, and a note for a message saying a key is missing: that
// the system keychain could not be read, when it could not (see
// keychainNote), or "".
func (a *App) savedKeys(rcpts []string) (saved []savedKey, note string) {
	for _, rcpt := range rcpts {
		s, id, err := a.storedKey(rcpt)
		note = cmp.Or(note, keychainNote(err))
		saved = append(saved, savedKey{rcpt: rcpt, secret: s, id: id, err: err})
	}
	return saved, note
}

// keychainNote says, for an error from the key store, that the system
// keychain could not be read, so the key may be saved there after all (see
// keys.KeychainError). It is "" for any other error.
func keychainNote(err error) string {
	var kc *keys.KeychainError
	if errors.As(err, &kc) {
		return kc.Why()
	}
	return ""
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
		data, err := repo.ReadSaltFile(r.Root, repo.KeyFile)
		if err != nil {
			return nil, secret, err
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
