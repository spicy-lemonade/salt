package keys

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/zalando/go-keyring"
)

func init() {
	keyring.MockInit() // never the real keychain
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
	if loc := fs.Location("age1x"); !filepath.IsAbs(loc) || filepath.Base(loc) != "age1x.json" {
		t.Fatalf("Location = %q", loc)
	}
}

type brokenStore struct{}

func (brokenStore) Get(string) (Secret, error) { return Secret{}, errors.New("no keychain") }
func (brokenStore) Set(string, Secret) error   { return errors.New("no keychain") }
func (brokenStore) Delete(string) error        { return errors.New("no keychain") }

func TestKeyringStoreMocked(t *testing.T) {
	keyring.MockInit() // in-memory; never the real keychain
	var st KeyringStore
	if _, err := st.Get("age1x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	e, _ := NewEntropy()
	if err := st.Set("age1x", PhraseSecret(e)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get("age1x"); err != nil || got != PhraseSecret(e) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := st.Delete("age1x"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("age1x"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	keyring.MockInitWithError(errors.New("locked"))
	if _, err := st.Get("age1x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get with a broken keychain: %v", err)
	}
	if err := st.Delete("age1x"); err == nil {
		t.Fatal("Delete with a broken keychain succeeded")
	}
	keyring.MockInit()
}

func TestErrorPaths(t *testing.T) {
	if _, err := EncodePhrase([]byte{1}); err == nil {
		t.Error("EncodePhrase accepted short entropy")
	}
	if _, err := IdentityFromEntropy([]byte{1}); err == nil {
		t.Error("IdentityFromEntropy accepted short entropy")
	}
	if _, err := UnwrapIdentity([]byte("not an age file"), "pass"); err == nil {
		t.Error("UnwrapIdentity accepted garbage")
	}
	if _, err := WrapIdentity(nil, ""); err == nil {
		t.Error("WrapIdentity accepted an empty passphrase")
	}
	for _, s := range []Secret{{Kind: "weird"}, {Kind: KindPhrase, Entropy: "zz"}, {Kind: KindIdentity, Key: "nope"}} {
		if _, err := s.Identity(); err == nil {
			t.Errorf("Secret %+v gave an identity", s)
		}
	}
	if _, err := (Secret{Kind: KindPhrase, Entropy: "zz"}).Phrase(); err == nil {
		t.Error("bad hex gave a phrase")
	}
	if _, err := LookupWord("ab"); err == nil {
		t.Error("two-letter word accepted")
	}
}

func TestFileStoreErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	fs := FileStore{Dir: blocker} // a file, not a directory
	if err := fs.Set("age1x", Secret{Kind: KindPhrase}); err == nil {
		t.Error("Set into a file path succeeded")
	}
	good := FileStore{Dir: dir}
	os.WriteFile(good.path("age1bad"), []byte("{"), 0o600)
	if _, err := good.Get("age1bad"); err == nil {
		t.Error("Get of a corrupt file succeeded")
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing", "f"), nil, 0o600); err == nil {
		t.Error("writeFileAtomic into a missing dir succeeded")
	}
	if err := (FileStore{Dir: filepath.Join(dir, "none")}).Delete("age1x"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestFallbackStoreMore(t *testing.T) {
	mem := &MemStore{}
	fs := FallbackStore{Primary: mem, Secondary: &MemStore{}}
	e, _ := NewEntropy()
	fs.Set("age1x", PhraseSecret(e))
	if fs.Location("age1x") != "" {
		t.Error("primary secret reported a fallback location")
	}
	fs2 := FallbackStore{Primary: brokenStore{}, Secondary: &MemStore{}}
	if loc := fs2.Location("age1x"); loc != "" {
		t.Errorf("Location = %q", loc)
	}
	if err := fs.Delete("age1x"); err != nil {
		t.Fatal(err)
	}
	if mem.Len() != 0 {
		t.Error("Delete left the secret")
	}
}
