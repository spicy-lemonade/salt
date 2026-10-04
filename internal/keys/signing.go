package keys

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"filippo.io/age"
)

// Domain separation for deriving the index-signing key from an age identity.
// Changing either string changes every signing key, so every existing backup
// would fail its signature check: never do it.
const (
	signSalt = "salt/index-signing/v1"
	signInfo = "ed25519 seed"
)

// KindSigning is a Secret holding the key that signs a backup's index.
const KindSigning = "signing"

// SigningKey derives the key that signs a backup's index from the age
// identity that decrypts it. The derivation is one-way, so the signing key
// can sign but never decrypt, and anyone who can decrypt a backup can work
// out the matching public key to check its signature.
func SigningKey(id *age.X25519Identity) (ed25519.PrivateKey, error) {
	seed, err := hkdf.Key(sha256.New, []byte(id.String()), []byte(signSalt), signInfo, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SigningSecret returns the secret that keeps a signing key.
func SigningSecret(k ed25519.PrivateKey) Secret {
	return Secret{Kind: KindSigning, Signing: hex.EncodeToString(k.Seed())}
}

// SigningKey returns the signing key the secret holds.
func (s Secret) SigningKey() (ed25519.PrivateKey, error) {
	if s.Kind != KindSigning {
		return nil, fmt.Errorf("not a signing key (kind %q)", s.Kind)
	}
	seed, err := hex.DecodeString(s.Signing)
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key has the wrong length")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
