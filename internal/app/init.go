package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/trust"
)

// InitOptions configures Init.
type InitOptions struct {
	Repo       string
	PlainPaths bool
	// Recovery is repo.RecoveryPhrase, repo.RecoveryPassphrase, or empty to ask.
	Recovery string
	// PassphraseFile supplies the passphrase for scripted installs.
	PassphraseFile string
}

const recoveryMenu = `How do you want to recover your backups if this laptop is lost?

  1) Recovery phrase  (recommended)
     salt generates 12 words for you to write down.
     The words are your decryption key. With this method, no decryption key
     is stored in your backup repo.

  2) Passphrase
     You choose a passphrase. An encrypted copy of your key is stored in your
     backup repo. Ensure it is strong because anyone with access to the repo
     can try to guess your password. Consider using a password manager like
     Bitwarden.

`

const phraseWarning = `Write these down, in order.
Anybody with these words can decrypt and read your backups.
If you lose them and this laptop, your backups cannot be recovered.
`

const passphraseWarning = `Ensure it is strong because anyone with access to the repo can try to guess
your password. Consider using a password manager like Bitwarden.
`

// Init sets up salt in a backup repository. Nothing is saved (keychain, repo
// files, hook) until the person has proved they recorded their recovery
// phrase or passphrase, so an aborted init leaves no key behind.
func (a *App) Init(o InitOptions) error {
	root, err := filepath.Abs(o.Repo)
	if err != nil {
		return err
	}
	if err := a.requireGitRepo(root); err != nil {
		return err
	}
	// A symlink committed by someone else could send what init writes into
	// another folder or file. Refuse before asking anything, so nobody writes
	// down a phrase or saves a key for a repo init then refuses.
	if err := seal.CheckNoSymlinks(root, ".gitattributes"); err != nil {
		return err
	}
	if _, err := repo.Open(root); err == nil {
		return fmt.Errorf("%s is already set up for salt; use `salt recovery test` to check your recovery phrase or passphrase", a.short(root))
	} else if !errors.Is(err, repo.ErrNotInitialised) {
		return err
	}

	method := o.Recovery
	if method == "" {
		if !a.UI.Interactive() {
			return fmt.Errorf("%w; pass --recovery phrase or --recovery passphrase", ErrNotInteractive)
		}
		if method, err = a.chooseRecovery(); err != nil {
			return err
		}
	}

	var (
		id      *age.X25519Identity
		secret  keys.Secret
		keyFile []byte
	)
	switch method {
	case repo.RecoveryPhrase:
		if !a.UI.Interactive() {
			return fmt.Errorf("%w: a recovery phrase has to be written down by a person, so run `salt init` in a terminal", ErrNotInteractive)
		}
		entropy, err := keys.NewEntropy()
		if err != nil {
			return err
		}
		if err := a.confirmPhrase(entropy); err != nil {
			return err
		}
		if id, err = keys.IdentityFromEntropy(entropy); err != nil {
			return err
		}
		secret = keys.PhraseSecret(entropy)
	case repo.RecoveryPassphrase:
		p, err := a.newPassphrase(o.PassphraseFile)
		if err != nil {
			return err
		}
		if id, err = age.GenerateX25519Identity(); err != nil {
			return err
		}
		if keyFile, err = keys.WrapIdentity(id, p); err != nil {
			return err
		}
		secret = keys.IdentitySecret(id)
	default:
		return fmt.Errorf("unknown recovery method %q (use phrase or passphrase)", method)
	}

	// From here on, things are saved. The key goes first: a repo whose key
	// was never saved would be unusable.
	rcpt := id.Recipient().String()
	if err := a.Store.Set(rcpt, secret); err != nil {
		return fmt.Errorf("saving your key: %w", err)
	}
	if err := a.saveSigningKey(id); err != nil {
		return err
	}
	if keyFile != nil {
		if err := writeKeyFile(root, keyFile); err != nil {
			return err
		}
	}
	f := repo.Format{Version: repo.FormatVersion, EncryptPaths: !o.PlainPaths, Recovery: method}
	if err := repo.Write(root, f, []string{rcpt}); err != nil {
		return err
	}
	if err := a.trustStore().Save(root, trust.Pin{Recipients: []string{rcpt}, EncryptPaths: f.EncryptPaths, Recovery: f.Recovery}); err != nil {
		return fmt.Errorf("saving the approved keys: %w", err)
	}
	if err := ensureGitattributes(root); err != nil {
		return err
	}
	hookPath, err := a.InstallHook(root)
	switch {
	case errors.Is(err, hook.ErrForeign):
		a.UI.Printf("\nNote: %v\n", err)
	case err != nil:
		return fmt.Errorf("installing the pre-commit hook: %w", err)
	}

	a.UI.Printf(`
salt is set up in %s
  Your key is saved in the %s.
  File paths are %s.
  Pre-commit hook: %s

Next:
  1. Commit the salt settings:
       git -C %q add .salt .gitattributes && git -C %q commit -m "Set up salt"
  2. In your backup script, replace the lines that copy files into the repo with:
       salt seal --prune "$STAGE" %q
  3. After the first backup, check you can restore:
       salt restore %q --to /tmp/salt-restore-test
`, a.short(root), a.StoreName, map[bool]string{true: "encrypted", false: "visible (--plain-paths)"}[f.EncryptPaths],
		a.short(hookPath), root, root, root, root)
	return nil
}

func (a *App) chooseRecovery() (string, error) {
	a.UI.Printf("%s", recoveryMenu)
	for {
		s, err := a.UI.ReadLine("Choose [1]: ")
		if err != nil {
			return "", err
		}
		switch strings.TrimSpace(s) {
		case "", "1":
			return repo.RecoveryPhrase, nil
		case "2":
			return repo.RecoveryPassphrase, nil
		}
		a.UI.Printf("Please type 1 or 2.\n")
	}
}

// confirmPhrase shows the phrase, hides it, and has the person type it back.
// A mistake restarts from the display; nothing has been saved yet.
func (a *App) confirmPhrase(entropy []byte) error {
	words, err := keys.EncodePhrase(entropy)
	if err != nil {
		return err
	}
	for {
		a.UI.Printf("\n%s\n%s\n", phraseWarning, formatPhrase(words))
		if _, err := a.UI.ReadLine("Press Enter once you have written them down. They will then be hidden from the screen."); err != nil {
			return err
		}
		a.UI.Clear()
		a.UI.Printf("Now check you wrote them down correctly.\n")
		got, err := a.readPhrase()
		if err == nil && bytes.Equal(got, entropy) {
			a.UI.Printf("✓ Your recovery phrase is correct.\n")
			return nil
		}
		if err == nil {
			err = errors.New("those words are a valid phrase, but not the one shown")
		}
		if errors.Is(err, errAborted) {
			return err
		}
		a.UI.Printf("\n✗ %v.\nLet's start again. Nothing has been saved yet.\n", err)
		if _, err := a.UI.ReadLine("Press Enter to see your words again."); err != nil {
			return err
		}
	}
}

var errAborted = errors.New("aborted")

func (a *App) readPhrase() ([]byte, error) {
	s, err := a.UI.ReadLine("Type your 12 words, separated by spaces:\n> ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAborted, err)
	}
	return keys.DecodePhrase(keys.SplitPhrase(s))
}

// formatPhrase lays the words out in three numbered columns.
func formatPhrase(words []string) string {
	rows := (len(words) + 2) / 3
	var b strings.Builder
	for r := 0; r < rows; r++ {
		b.WriteString("  ")
		for c := 0; c < 3; c++ {
			i := c*rows + r
			if i < len(words) {
				fmt.Fprintf(&b, " %2d. %-10s", i+1, words[i])
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (a *App) newPassphrase(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		p := strings.TrimRight(string(b), "\r\n")
		if err := keys.CheckPassphrase(p); err != nil {
			return "", fmt.Errorf("passphrase in %s: %w", file, err)
		}
		return p, nil
	}
	if !a.UI.Interactive() {
		return "", fmt.Errorf("%w; or pass --passphrase-file", ErrNotInteractive)
	}
	a.UI.Printf("\n%s\n", passphraseWarning)
	for {
		p, err := a.UI.ReadSecret("Choose a passphrase: ")
		if err != nil {
			return "", err
		}
		if err := keys.CheckPassphrase(p); err != nil {
			a.UI.Printf("✗ Too weak: %v.\n", err)
			continue
		}
		again, err := a.UI.ReadSecret("Type it again to confirm: ")
		if err != nil {
			return "", err
		}
		if again != p {
			a.UI.Printf("✗ Those don't match. Let's start again. Nothing has been saved yet.\n")
			continue
		}
		a.UI.Printf("✓ Passphrase set.\n")
		return p, nil
	}
}

// writeKeyFile saves the passphrase-wrapped key through os.Root, so a symlink
// in the repo can't lead it outside.
func writeKeyFile(root string, keyFile []byte) error {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.MkdirAll(repo.Dir, 0o755); err != nil {
		return err
	}
	return rt.WriteFile(repo.KeyFile, keyFile, 0o644)
}

// ensureGitattributes marks ciphertext as binary so git never tries to diff
// or merge it as text. It reads the file as salt reads its own in .salt, so
// a symlink, a named pipe or a file far larger than salt writes is refused,
// and writes it through os.Root, so a symlink can't lead it to a file
// outside the repo.
func ensureGitattributes(root string) error {
	const p = ".gitattributes"
	b, err := repo.ReadSaltFile(root, p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	if strings.Contains(string(b), "*.age binary") {
		return nil
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	return rt.WriteFile(p, append(b, "*.age binary\n"...), 0o644)
}
