package keys

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
)

func init() {
	// Keep scrypt cheap in tests: 2^18 would use 256 MiB per wrap.
	WrapWorkFactor = 10
}

func TestWordlist(t *testing.T) {
	if len(wordlist) != 2048 || wordlist[0] != "abandon" || wordlist[2047] != "zoo" {
		t.Fatalf("wordlist: %d words, first %q, last %q", len(wordlist), wordlist[0], wordlist[len(wordlist)-1])
	}
	prefixes := map[string]string{}
	for _, w := range wordlist {
		p := w[:min(4, len(w))]
		if prev, ok := prefixes[p]; ok {
			t.Fatalf("%q and %q share prefix %q", prev, w, p)
		}
		prefixes[p] = w
	}
}

// BIP39 reference vectors (entropy -> mnemonic).
func TestEncodePhraseVectors(t *testing.T) {
	vectors := map[string]string{
		"00000000000000000000000000000000": "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f": "legal winner thank year wave sausage worth useful legal winner thank yellow",
		"ffffffffffffffffffffffffffffffff": "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong",
	}
	for hexEnt, want := range vectors {
		e, _ := hex.DecodeString(hexEnt)
		got, err := EncodePhrase(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("EncodePhrase(%s) = %q, want %q", hexEnt, strings.Join(got, " "), want)
		}
		back, err := DecodePhrase(strings.Fields(want))
		if err != nil || !bytes.Equal(back, e) {
			t.Errorf("DecodePhrase(%q) = %x, %v", want, back, err)
		}
	}
}

func TestDecodePhraseInput(t *testing.T) {
	// Prefixes and case are accepted.
	if _, err := DecodePhrase(SplitPhrase("LEGA winn than year wave saus worth usef legal winner thank yell")); err != nil {
		t.Fatalf("prefix input: %v", err)
	}
	// Swapped words fail the checksum.
	if _, err := DecodePhrase(strings.Fields("winner legal thank year wave sausage worth useful legal winner thank yellow")); !errors.Is(err, ErrChecksum) {
		t.Fatalf("swapped words: err = %v, want ErrChecksum", err)
	}
	if _, err := DecodePhrase(strings.Fields("abandon notaword abandon abandon abandon abandon abandon abandon abandon abandon abandon about")); err == nil || !strings.Contains(err.Error(), "word 2") {
		t.Fatalf("bad word: err = %v", err)
	}
	if _, err := DecodePhrase([]string{"abandon"}); err == nil {
		t.Fatal("short phrase accepted")
	}
}

func TestIdentityFromEntropyDeterministic(t *testing.T) {
	e := bytes.Repeat([]byte{7}, 16)
	a, err := IdentityFromEntropy(e)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := IdentityFromEntropy(e)
	if a.String() != b.String() {
		t.Fatal("derivation is not deterministic")
	}
	other, _ := IdentityFromEntropy(bytes.Repeat([]byte{8}, 16))
	if other.String() == a.String() {
		t.Fatal("different entropy gave the same identity")
	}
	roundTrip(t, a)
}

// Pinned so an accidental change to the derivation is caught: it would make
// every existing recovery phrase useless.
func TestIdentityFromEntropyPinned(t *testing.T) {
	id, err := IdentityFromEntropy(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	const want = "age10926w6w5nran64yccra07fm39a6dn9x9tnpr6exdwp4cmnfpjd7svrnmlw"
	if got := id.Recipient().String(); got != want {
		t.Fatalf("recipient for zero entropy = %s, want %s", got, want)
	}
}

func TestWrapUnwrap(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	data, err := WrapIdentity(id, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapIdentity(data, "correct horse battery staple")
	if err != nil || got.String() != id.String() {
		t.Fatalf("unwrap: %v", err)
	}
	if _, err := UnwrapIdentity(data, "wrong horse battery staple"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: err = %v", err)
	}
}

func TestCheckPassphrase(t *testing.T) {
	for p, ok := range map[string]bool{
		"short":                        false,
		"aaaaaaaaaaaaaaaaaaaa":         false,
		"abababababababababab":         false,
		"correct horse battery staple": true,
	} {
		if err := CheckPassphrase(p); (err == nil) != ok {
			t.Errorf("CheckPassphrase(%q) = %v", p, err)
		}
	}
}

func TestSecrets(t *testing.T) {
	e, _ := NewEntropy()
	ps := PhraseSecret(e)
	words, err := ps.Phrase()
	if err != nil || len(words) != PhraseWords {
		t.Fatalf("Phrase() = %v, %v", words, err)
	}
	id, _ := age.GenerateX25519Identity()
	if _, err := IdentitySecret(id).Phrase(); err == nil {
		t.Fatal("identity secret returned a phrase")
	}
	for name, st := range map[string]Store{"mem": &MemStore{}, "file": FileStore{Dir: t.TempDir()}} {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Get("age1x"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get missing: %v", err)
			}
			if err := st.Set("age1x", ps); err != nil {
				t.Fatal(err)
			}
			got, err := st.Get("age1x")
			if err != nil || got != ps {
				t.Fatalf("Get = %+v, %v", got, err)
			}
			if err := st.Delete("age1x"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Get("age1x"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after delete: %v", err)
			}
		})
	}
}

func roundTrip(t *testing.T, id *age.X25519Identity) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	w.Close()
	r, err := age.Decrypt(&buf, id)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.ReadFrom(r)
	if out.String() != "hello" {
		t.Fatalf("round trip = %q", out.String())
	}
}

func TestFallbackStore(t *testing.T) {
	broken := brokenStore{}
	file := FileStore{Dir: t.TempDir()}
	var warned error
	fs := FallbackStore{Primary: broken, Secondary: file, Warn: func(err error) { warned = err }}
	id, _ := age.GenerateX25519Identity()
	if err := fs.Set("age1x", IdentitySecret(id)); err != nil || warned == nil {
		t.Fatalf("Set: %v, warned %v", err, warned)
	}
	if _, err := fs.Get("age1x"); err != nil {
		t.Fatal(err)
	}
	if loc := fs.Location("age1x"); !strings.HasPrefix(loc, "private file ") {
		t.Fatalf("Location = %q", loc)
	}
}

type brokenStore struct{}

func (brokenStore) Get(string) (Secret, error) { return Secret{}, errors.New("no keychain") }
func (brokenStore) Set(string, Secret) error   { return errors.New("no keychain") }
func (brokenStore) Delete(string) error        { return errors.New("no keychain") }
