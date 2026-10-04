package seal

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

const planted = "planted by someone with only the public key\n"

// plant replaces the repo's index with one listing a file encrypted by
// someone who has only the public key, as anyone who can push could. edit
// changes the index before it is written, to sign or tamper with it.
func (f *fixture) plant(edit func(*Index)) {
	f.t.Helper()
	rt, err := os.OpenRoot(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rt.Close()
	const obj = "objects/pl/anted.age"
	sha, err := encryptTo(rt, obj, strings.NewReader(planted), f.repo.Recipients)
	if err != nil {
		f.t.Fatal(err)
	}
	ix := &Index{Version: repo.FormatVersion, Entries: []Entry{
		{Path: "MEMORY.md", Object: obj, SHA256: sha, Size: int64(len(planted)), Mode: 0o644},
	}}
	edit(ix)
	b, _, err := ix.marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		f.t.Fatal(err)
	}
}

func signWith(k ed25519.PrivateKey) func(*Index) {
	return func(ix *Index) { ix.sign(k) }
}

func TestSealSignsIndex(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Signature == "" || ix.Unsigned {
		t.Fatalf("index signature %q, unsigned %v", ix.Signature, ix.Unsigned)
	}
	// The signature is the same every time, so an unchanged snapshot still
	// leaves the index alone.
	if res := f.seal(false); res.IndexNew {
		t.Fatal("an unchanged snapshot rewrote the signed index")
	}
}

func TestSealNeedsSigner(t *testing.T) {
	f := newFixture(t, true)
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil || !strings.Contains(err.Error(), "no signing key") {
		t.Fatalf("seal without a signing key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, repo.IndexFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seal without a signing key wrote an index: %v", err)
	}
}

// A planted index is refused by Restore, Verify and ReadIndex unless it is
// signed by the signing key of the identity that opens it.
func TestPlantedIndexRefused(t *testing.T) {
	other, _ := age.GenerateX25519Identity()
	otherKey := keys.SigningKey(other)
	for name, tt := range map[string]struct {
		edit func(*testing.T, *fixture) func(*Index)
		want string
	}{
		"no signature": {func(*testing.T, *fixture) func(*Index) { return func(*Index) {} }, "it has no signature"},
		"another key":  {func(t *testing.T, _ *fixture) func(*Index) { return signWith(otherKey) }, "does not match"},
		"not base64":   {func(*testing.T, *fixture) func(*Index) { return func(ix *Index) { ix.Signature = "!!" } }, "malformed"},
		"short":        {func(*testing.T, *fixture) func(*Index) { return func(ix *Index) { ix.Signature = "AAAA" } }, "malformed"},
		"edited after signing": {func(t *testing.T, f *fixture) func(*Index) {
			return func(ix *Index) {
				signWith(f.signer)(ix)
				ix.Entries[0].Path = "SOUL.md"
			}
		}, "does not match"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			f.seal(false)
			f.plant(tt.edit(t, f))

			if _, err := ReadIndex(f.root, f.ids(), false); !errors.Is(err, ErrNotSigned) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadIndex: %v, want %q", err, tt.want)
			}
			dest := filepath.Join(t.TempDir(), "r")
			if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); !errors.Is(err, ErrNotSigned) {
				t.Fatalf("Restore: %v", err)
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused restore wrote %s: %v", dest, err)
			}
			if _, err := Verify(f.root, f.ids(), VerifyOptions{}); !errors.Is(err, ErrNotSigned) {
				t.Fatalf("Verify: %v", err)
			}

			// Allowed, the files are still checked against the index, and
			// the result says the index was not signed.
			vr, err := Verify(f.root, f.ids(), VerifyOptions{AllowUnsigned: true})
			if err != nil || !vr.Unsigned || vr.ProblemCount != 0 {
				t.Fatalf("Verify allowing unsigned: %+v, %v", vr, err)
			}
			rr, err := Restore(f.root, f.ids(), dest, RestoreOptions{AllowUnsigned: true})
			if err != nil || !rr.Unsigned {
				t.Fatalf("Restore allowing unsigned: %+v, %v", rr, err)
			}
		})
	}
}

// Without allowUnsigned an unsigned index is refused; with it, other errors
// are still errors.
func TestAllowUnsignedKeepsOtherChecks(t *testing.T) {
	f := newFixture(t, true)
	body := `{"version":1,"entries":[],"signature":5}`
	if err := writeIndex(f.root, []byte(body), f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(f.root, f.ids(), true); err == nil || errors.Is(err, ErrNotSigned) || !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("signature that is not a string: %v", err)
	}
	f.writeIndex(&Index{Version: 2, Entries: []Entry{}})
	if _, err := ReadIndex(f.root, f.ids(), true); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("unsupported version: %v", err)
	}
}

// Identities that are not X25519 keys have no signing key and are skipped.
func TestSignatureSkipsOtherIdentities(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	pw, err := age.NewScryptIdentity("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(f.root, []age.Identity{pw, f.id}, false); err != nil {
		t.Fatalf("with a passphrase identity first: %v", err)
	}
}

// The order of the index's fields does not matter, but the order of the
// entries does.
func TestSignatureCoversEntryOrder(t *testing.T) {
	f := newFixture(t, true)
	f.write("b.md", "b\n")
	f.seal(false)
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		t.Fatal(err)
	}
	ix.Entries[0], ix.Entries[1] = ix.Entries[1], ix.Entries[0]
	b, _, err := ix.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(f.root, f.ids(), false); !errors.Is(err, ErrNotSigned) {
		t.Fatalf("reordered entries: %v", err)
	}
}

// JSON stores a file name that is not valid UTF-8 with U+FFFD in place of
// the bad bytes. The signature must cover the index as it is stored, or such
// a backup could never be restored.
func TestSignatureWithNonUTF8Name(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		f := newFixture(t, encryptPaths)
		name := "bad\xffname.md"
		if err := os.WriteFile(filepath.Join(f.src, name), []byte("x"), 0o644); err != nil {
			t.Skipf("this file system refuses a name that is not UTF-8: %v", err)
		}
		f.seal(false)
		if _, err := ReadIndex(f.root, f.ids(), false); err != nil {
			t.Fatalf("encryptPaths %v: %v", encryptPaths, err)
		}
	}
}
