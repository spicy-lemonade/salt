package keys

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
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
func SigningKey(id *age.X25519Identity) ed25519.PrivateKey {
	// hkdf.Key fails only for keys longer than 255 SHA-256 blocks, far more
	// than the 32 bytes asked for here.
	seed, _ := hkdf.Key(sha256.New, []byte(id.String()), []byte(signSalt), signInfo, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

// SigningSecret returns the secret that keeps a signing key.
func SigningSecret(k ed25519.PrivateKey) Secret {
	return Secret{Kind: KindSigning, Signing: hex.EncodeToString(k.Seed())}
}

// SigningKey returns the signing key the secret holds. A secret that does
// not hold one is ErrDamaged. Why is said without quoting any of it, as it
// may be part of the key.
func (s Secret) SigningKey() (ed25519.PrivateKey, error) {
	if s.Kind != KindSigning {
		return nil, fmt.Errorf("%w: it is not a signing key", ErrDamaged)
	}
	seed, err := hex.DecodeString(s.Signing)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: it is not %d bytes in hex", ErrDamaged, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
