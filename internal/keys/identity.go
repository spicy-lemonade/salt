package keys

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"filippo.io/age"
)

// Domain separation for deriving an age identity from phrase entropy.
// Changing either string changes every derived key: never do it.
const (
	deriveSalt = "salt/recovery-phrase/v1"
	deriveInfo = "age X25519 identity"
)

// IdentityFromEntropy derives the age identity for a recovery phrase.
func IdentityFromEntropy(entropy []byte) (*age.X25519Identity, error) {
	if len(entropy) != entropyBytes {
		return nil, fmt.Errorf("entropy must be %d bytes", entropyBytes)
	}
	scalar, err := hkdf.Key(sha256.New, entropy, []byte(deriveSalt), deriveInfo, 32)
	if err != nil {
		return nil, err
	}
	s, err := bech32Encode("age-secret-key-", scalar)
	if err != nil {
		return nil, err
	}
	return age.ParseX25519Identity(strings.ToUpper(s))
}

// WrapWorkFactor is the scrypt work factor (log2 N) for passphrase-wrapped
// keys: about a second per guess and 256 MiB of memory.
var WrapWorkFactor = 18

// maxUnwrapWorkFactor bounds the memory a crafted key.age can make unwrap use.
const maxUnwrapWorkFactor = 18

// WrapIdentity encrypts an identity with a passphrase (the key.age file).
func WrapIdentity(id *age.X25519Identity, passphrase string) ([]byte, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(WrapWorkFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, id.String()+"\n"); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrWrongPassphrase is returned when key.age does not open.
var ErrWrongPassphrase = errors.New("wrong passphrase")

// UnwrapIdentity opens a key.age file.
func UnwrapIdentity(data []byte, passphrase string) (*age.X25519Identity, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(maxUnwrapWorkFactor)
	r, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrWrongPassphrase
		}
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return nil, err
	}
	return age.ParseX25519Identity(strings.TrimSpace(string(b)))
}

// MinPassphraseLen is the shortest passphrase salt accepts.
const MinPassphraseLen = 16

// CheckPassphrase rejects passphrases that are easy to guess offline.
func CheckPassphrase(p string) error {
	runes := []rune(p)
	if len(runes) < MinPassphraseLen {
		return fmt.Errorf("use at least %d characters; a password manager like Bitwarden can generate one", MinPassphraseLen)
	}
	distinct := map[rune]bool{}
	for _, r := range runes {
		distinct[unicode.ToLower(r)] = true
	}
	if len(distinct) < 8 {
		return errors.New("too repetitive; use a longer mix of words or characters")
	}
	return nil
}
