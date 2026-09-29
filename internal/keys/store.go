package keys

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/zalando/go-keyring"
)

// Secret is what salt keeps in the keychain for one key. Phrase-based keys
// store the entropy, so the phrase can be shown again (salt recovery show);
// passphrase-based keys store the identity itself.
type Secret struct {
	Kind    string `json:"kind"` // KindPhrase or KindIdentity
	Entropy string `json:"entropy,omitempty"`
	Key     string `json:"identity,omitempty"`
}

const (
	KindPhrase   = "phrase"
	KindIdentity = "identity"
)

// PhraseSecret returns the secret for a recovery-phrase key.
func PhraseSecret(entropy []byte) Secret {
	return Secret{Kind: KindPhrase, Entropy: hex.EncodeToString(entropy)}
}

// IdentitySecret returns the secret for a passphrase-wrapped key.
func IdentitySecret(id *age.X25519Identity) Secret {
	return Secret{Kind: KindIdentity, Key: id.String()}
}

// Identity returns the age identity the secret holds.
func (s Secret) Identity() (*age.X25519Identity, error) {
	switch s.Kind {
	case KindPhrase:
		e, err := hex.DecodeString(s.Entropy)
		if err != nil {
			return nil, err
		}
		return IdentityFromEntropy(e)
	case KindIdentity:
		return age.ParseX25519Identity(s.Key)
	}
	return nil, fmt.Errorf("unknown secret kind %q", s.Kind)
}

// Phrase returns the recovery phrase, for phrase-based keys only.
func (s Secret) Phrase() ([]string, error) {
	if s.Kind != KindPhrase {
		return nil, errors.New("this key uses a passphrase, not a recovery phrase")
	}
	e, err := hex.DecodeString(s.Entropy)
	if err != nil {
		return nil, err
	}
	return EncodePhrase(e)
}

// ErrNotFound means the store has no secret for that recipient.
var ErrNotFound = errors.New("key not found")

// Store keeps secrets keyed by their public recipient string.
type Store interface {
	Get(recipient string) (Secret, error)
	Set(recipient string, s Secret) error
	Delete(recipient string) error
}

// KeyringService is the OS keychain service name salt uses.
const KeyringService = "salt"

// KeyringStore uses the OS keychain (macOS Keychain, Linux Secret Service,
// Windows Credential Manager).
type KeyringStore struct{}

func (KeyringStore) Get(recipient string) (Secret, error) {
	v, err := keyring.Get(KeyringService, recipient)
	if errors.Is(err, keyring.ErrNotFound) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	var s Secret
	return s, json.Unmarshal([]byte(v), &s)
}

func (KeyringStore) Set(recipient string, s Secret) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return keyring.Set(KeyringService, recipient, string(b))
}

func (KeyringStore) Delete(recipient string) error {
	err := keyring.Delete(KeyringService, recipient)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// FileStore keeps secrets as 0600 files in Dir. It is the fallback for
// machines without a keychain (headless Linux).
type FileStore struct{ Dir string }

func (f FileStore) path(recipient string) string {
	return filepath.Join(f.Dir, strings.ToLower(recipient)+".json")
}

func (f FileStore) Get(recipient string) (Secret, error) {
	b, err := os.ReadFile(f.path(recipient))
	if errors.Is(err, os.ErrNotExist) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	var s Secret
	return s, json.Unmarshal(b, &s)
}

func (f FileStore) Set(recipient string, s Secret) error {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(f.path(recipient), b, 0o600)
}

func (f FileStore) Delete(recipient string) error {
	err := os.Remove(f.path(recipient))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu sync.Mutex
	m  map[string]Secret
}

func (m *MemStore) Get(recipient string) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.m[recipient]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return s, nil
}

func (m *MemStore) Set(recipient string, s Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]Secret{}
	}
	m.m[recipient] = s
	return nil
}

func (m *MemStore) Delete(recipient string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, recipient)
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// FallbackStore uses Primary (the OS keychain) and falls back to Secondary
// (a 0600 file) when the keychain is unavailable, e.g. on headless Linux.
type FallbackStore struct {
	Primary, Secondary Store
	// Warn is told when Set had to use Secondary.
	Warn func(error)
}

func (f FallbackStore) Get(recipient string) (Secret, error) {
	s, err := f.Primary.Get(recipient)
	if err == nil {
		return s, nil
	}
	return f.Secondary.Get(recipient)
}

func (f FallbackStore) Set(recipient string, s Secret) error {
	err := f.Primary.Set(recipient, s)
	if err == nil {
		return nil
	}
	if f.Warn != nil {
		f.Warn(err)
	}
	return f.Secondary.Set(recipient, s)
}

func (f FallbackStore) Delete(recipient string) error {
	err1 := f.Primary.Delete(recipient)
	err2 := f.Secondary.Delete(recipient)
	return errors.Join(err1, err2)
}

// Len reports how many secrets the store holds.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}
