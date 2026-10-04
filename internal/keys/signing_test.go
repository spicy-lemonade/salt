package keys

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"filippo.io/age"
)

// Changing signSalt or signInfo would fail the signature check of every
// existing backup, so the derivation is pinned.
func TestSigningKeyPinned(t *testing.T) {
	id, err := IdentityFromEntropy(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	k, err := SigningKey(id)
	if err != nil {
		t.Fatal(err)
	}
	const want = "e4b62dcc47820129cadeb009161b76511c45c73e96175143788ca18669a39a86"
	if got := hex.EncodeToString(k.Public().(ed25519.PublicKey)); got != want {
		t.Fatalf("signing key for zero entropy = %s, want %s", got, want)
	}
}

func TestSigningKeyDiffersPerIdentity(t *testing.T) {
	a, _ := age.GenerateX25519Identity()
	b, _ := age.GenerateX25519Identity()
	ka, _ := SigningKey(a)
	kb, _ := SigningKey(b)
	again, _ := SigningKey(a)
	if ka.Equal(kb) {
		t.Fatal("two identities gave the same signing key")
	}
	if !ka.Equal(again) {
		t.Fatal("the same identity gave two signing keys")
	}
}

func TestSigningSecret(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	k, _ := SigningKey(id)
	got, err := SigningSecret(k).SigningKey()
	if err != nil || !got.Equal(k) {
		t.Fatalf("round trip: %v", err)
	}
	// The signing key is kept as a seed, never as the decryption key.
	if s := SigningSecret(k); s.Key != "" || s.Entropy != "" {
		t.Fatalf("signing secret holds more than the seed: %+v", s)
	}
	for name, s := range map[string]Secret{
		"wrong kind":   {Kind: KindIdentity, Signing: hex.EncodeToString(k.Seed())},
		"not hex":      {Kind: KindSigning, Signing: "zz"},
		"wrong length": {Kind: KindSigning, Signing: "abcd"},
	} {
		if _, err := s.SigningKey(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSigningSecretInFileStore(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	k, _ := SigningKey(id)
	fs := FileStore{Dir: t.TempDir()}
	rcpt := id.Recipient().String()
	if err := fs.Set(rcpt, SigningSecret(k)); err != nil {
		t.Fatal(err)
	}
	s, err := fs.Get(rcpt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SigningKey(); err != nil || !got.Equal(k) {
		t.Fatalf("from the file store: %v", err)
	}
}
