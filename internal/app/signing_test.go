package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// signingFile is where the test env keeps the signing key for the repo's
// first recipient.
func signingFile(t *testing.T, e *testEnv) string {
	t.Helper()
	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(e.app.SignDir, r.RecipientStrings[0]+".json")
}

func TestInitSavesSigningKey(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, _ := repo.Open(e.root)
	rcpt := r.RecipientStrings[0]
	s, err := e.app.signStore().Get(rcpt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SigningKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := e.store.Get(rcpt)
	id, _ := secret.Identity()
	if want := keys.SigningKey(id); !got.Equal(want) {
		t.Fatal("the saved signing key is not the one derived from the key")
	}
	fi, err := os.Stat(signingFile(t, e))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("signing key file mode %v, want 0600", fi.Mode().Perm())
	}
	// The decryption key is not in the signing store.
	b, _ := os.ReadFile(signingFile(t, e))
	if strings.Contains(string(b), "AGE-SECRET-KEY") || strings.Contains(string(b), secret.Entropy) {
		t.Fatal("the signing key file holds the decryption key")
	}
}

func TestInitWithUnwritableSigningStore(t *testing.T) {
	e := newEnv(t)
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o600)
	e.app.SignDir = file
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err == nil || !strings.Contains(err.Error(), "saving the signing key") {
		t.Fatalf("init with an unwritable signing store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, repo.Dir)); !os.IsNotExist(err) {
		t.Fatal("init wrote .salt without a signing key")
	}
}

// A machine without a signing key (such as one set up before salt signed
// backups) is told how to set one up, and `salt trust` does it.
func TestSealNeedsSigningKey(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	os.RemoveAll(e.app.SignDir)
	src := t.TempDir()
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root})
	if err == nil || !strings.Contains(err.Error(), "no key to sign backups") || !strings.Contains(err.Error(), "salt trust") {
		t.Fatalf("seal without a signing key: %v", err)
	}
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "! this machine has no key to sign backups") {
		t.Fatalf("doctor without a signing key:\n%s", e.ui.out.String())
	}
	e.ui.out.Reset()
	if err := e.app.Trust(e.root, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "can now sign backups") {
		t.Fatalf("trust output:\n%s", e.ui.out.String())
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); err != nil {
		t.Fatalf("seal after trust: %v", err)
	}
	if err := e.app.Verify(e.root, false); err != nil {
		t.Fatalf("verify after trust: %v", err)
	}
	// Approving again leaves the signing key alone.
	e.ui.out.Reset()
	if err := e.app.Trust(e.root, true); err != nil || strings.Contains(e.ui.out.String(), "can now sign") {
		t.Fatalf("second trust: %v\n%s", err, e.ui.out.String())
	}
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "✓ this machine can sign backups") {
		t.Fatalf("doctor with a signing key:\n%s", e.ui.out.String())
	}
}

// On a machine with neither key, trust needs the recovery phrase, and
// approves nothing without it.
func TestTrustSetsUpSigningFromThePhrase(t *testing.T) {
	e := newEnv(t)
	phrase := healthyRepo(t, e)
	os.RemoveAll(e.app.SignDir)
	e.app.Store = &keys.MemStore{}
	e.app.TrustDir = filepath.Join(t.TempDir(), "fresh")

	e.ui.interactive = false
	if err := e.app.Trust(e.root, true); err == nil || !strings.Contains(err.Error(), "set up signing") {
		t.Fatalf("trust --yes without any key: %v", err)
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("a failed trust approved the repo: %v", err)
	}

	e.ui.interactive = true
	e.ui.answer = func(p, _ string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			return phrase, nil
		}
		if strings.HasPrefix(p, "Save the key") {
			return "n", nil
		}
		return "y", nil
	}
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatalf("trust with the phrase: %v\n%s", err, e.ui.out.String())
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err != nil {
		t.Fatalf("seal after trust: %v", err)
	}
	// Only the signing key was kept: the person said no to saving the key.
	if e.app.Store.(*keys.MemStore).Len() != 0 {
		t.Fatal("the decryption key was saved")
	}
}

func TestSigningKeyStoreErrors(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	os.WriteFile(signingFile(t, e), []byte("not json"), 0o600)
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err == nil || !strings.Contains(err.Error(), "reading the signing key") {
		t.Fatalf("seal with a damaged signing key: %v", err)
	}
	if err := e.app.Trust(e.root, true); err == nil {
		t.Fatal("trust with a damaged signing key succeeded")
	}
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "✗ the signing key saved on this machine cannot be read") {
		t.Fatalf("doctor with a damaged signing key:\n%s", e.ui.out.String())
	}

	// A signing store that cannot be made (here, a link to a missing folder)
	// fails trust before approving, after the key was found in the keychain.
	e.app.SignDir = filepath.Join(t.TempDir(), "signing")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), e.app.SignDir); err != nil {
		t.Fatal(err)
	}
	e.app.TrustDir = filepath.Join(t.TempDir(), "fresh")
	if err := e.app.Trust(e.root, true); err == nil || !strings.Contains(err.Error(), "saving the signing key") {
		t.Fatalf("trust with a signing store that cannot be made: %v", err)
	}
	if _, err := e.app.trustStore().Load(e.root); err == nil {
		t.Fatal("trust approved the repo without saving the signing key")
	}

	// A signing store that cannot be read fails trust before approving.
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o600)
	e.app.SignDir = filepath.Join(file, "signing")
	e.app.TrustDir = filepath.Join(t.TempDir(), "fresh")
	if err := e.app.Trust(e.root, true); err == nil {
		t.Fatal("trust with an unwritable signing store succeeded")
	}
	if _, err := e.app.trustStore().Load(e.root); err == nil {
		t.Fatal("trust approved the repo without setting up signing")
	}
}

// plantBackup replaces the backup with one made by someone who can push to
// the repo but holds only its public key: they seal their own files to it,
// signed with their own key.
func plantBackup(t *testing.T, e *testEnv) {
	t.Helper()
	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := age.GenerateX25519Identity()
	signer := keys.SigningKey(theirs)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "USER.md"), []byte("planted"), 0o644)
	if _, err := seal.Seal(src, r, seal.Options{CacheDir: t.TempDir(), Signer: signer, Prune: true}); err != nil {
		t.Fatal(err)
	}
}

func TestPlantedBackupRefused(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	plantBackup(t, e)

	dest := filepath.Join(t.TempDir(), "restored")
	err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest})
	if !errors.Is(err, seal.ErrNotSigned) || !strings.Contains(err.Error(), "--allow-unsigned") || !strings.Contains(err.Error(), "plant files") {
		t.Fatalf("restore of a planted backup: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a refused restore wrote files")
	}
	if err := e.app.Verify(e.root, false); !errors.Is(err, seal.ErrNotSigned) || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("verify of a planted backup: %v", err)
	}

	// Allowed, both go ahead with a warning.
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, true); err != nil {
		t.Fatalf("verify --allow-unsigned: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "not signed by your key") {
		t.Fatalf("verify --allow-unsigned output:\n%s", e.ui.out.String())
	}
	e.ui.out.Reset()
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest, AllowUnsigned: true}); err != nil {
		t.Fatalf("restore --allow-unsigned: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "not signed by your key") {
		t.Fatalf("restore --allow-unsigned output:\n%s", e.ui.out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "USER.md")); string(b) != "planted" {
		t.Fatalf("restored %q", b)
	}

	// The owner's next seal signs the backup again.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "USER.md"), []byte("hello"), 0o644)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil {
		t.Fatal(err)
	}
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, false); err != nil || strings.Contains(e.ui.out.String(), "not signed") {
		t.Fatalf("verify after resealing: %v\n%s", err, e.ui.out.String())
	}
}

// A saved signing key that is not the one the saved decryption key derives,
// whose backups restore would refuse, is reported by doctor and replaced by
// salt trust.
func TestWrongSigningKeyIsReplaced(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	rcpt := r.RecipientStrings[0]
	other, _ := age.GenerateX25519Identity()
	if err := e.app.signStore().Set(rcpt, keys.SigningSecret(keys.SigningKey(other))); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), "the signing key saved on this machine does not match your key") {
		t.Fatalf("doctor: %v\n%s", err, e.ui.out.String())
	}
	if err := e.app.Trust(e.root, true); err != nil {
		t.Fatal(err)
	}
	id, err := e.app.storedIdentity(rcpt)
	if err != nil {
		t.Fatal(err)
	}
	if k, _, err := e.app.signingKey(r); err != nil || !k.Equal(keys.SigningKey(id)) {
		t.Fatalf("signing key after trust is still wrong: %v", err)
	}
	// One whose decryption key is not saved here cannot be checked, and is
	// kept.
	if err := e.app.Store.Delete(rcpt); err != nil {
		t.Fatal(err)
	}
	e.app.signStore().Set(rcpt, keys.SigningSecret(keys.SigningKey(other)))
	if k, rcpt, _ := e.app.signingKey(r); e.app.wrongSigningKey(k, rcpt) {
		t.Fatal("a key that cannot be checked counts as wrong")
	}
}

// A key saved for a public key that it is not the key for is damaged: trust
// does not call it the person's, and restore asks for the phrase instead.
func TestTrustMarksADamagedKey(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := age.GenerateX25519Identity()
	e.app.Store.Set(r.RecipientStrings[0], keys.IdentitySecret(other))
	e.ui.answer = func(p, out string) (string, error) { return "n", nil }
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	if strings.Contains(out, "(your key on this machine)") || !strings.Contains(out, "⚠ the key saved for it on this machine is damaged") {
		t.Fatalf("trust output:\n%s", out)
	}
	e.ui.out.Reset()
	e.app.identities(r)
	if !strings.Contains(e.ui.out.String(), "No key for this backup is saved on this machine") {
		t.Fatalf("identities used a damaged key:\n%s", e.ui.out.String())
	}
}
