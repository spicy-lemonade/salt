package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/repo"
)

type fixture struct {
	t     *testing.T
	src   string
	root  string
	cache string
	id    *age.X25519Identity
	repo  *repo.Repo
}

func newFixture(t *testing.T, encryptPaths bool) *fixture {
	t.Helper()
	base := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:     t,
		src:   filepath.Join(base, "src"),
		root:  filepath.Join(base, "repo"),
		cache: filepath.Join(base, "cache"),
		id:    id,
	}
	if err := repo.Write(f.root, repo.Format{Version: repo.FormatVersion, EncryptPaths: encryptPaths, Recovery: repo.RecoveryPhrase},
		[]string{id.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	if f.repo, err = repo.Open(f.root); err != nil {
		t.Fatal(err)
	}
	f.write("memories/USER.md", "The user is called Ciaran.\n")
	f.write("memories/MEMORY.md", "secret memory\n")
	f.write("SOUL.md", "be kind\n")
	f.write("skills/tax-return-2026/SKILL.md", "# Tax\n")
	f.write("mnemosyne/data/mnemosyne.db", "SQLite format 3\x00"+strings.Repeat("\x01\x02", 5000))
	if err := os.Symlink("SOUL.md", filepath.Join(f.src, "soul-link")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) seal(prune bool) *Result {
	f.t.Helper()
	res, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Prune: prune})
	if err != nil {
		f.t.Fatalf("Seal: %v", err)
	}
	return res
}

func (f *fixture) ids() []age.Identity { return []age.Identity{f.id} }

// snapshot maps every repo file (outside .salt) to its content hash.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if rel == repo.Dir {
				return filepath.SkipDir
			}
			return nil
		}
		b, _ := os.ReadFile(p)
		s := sha256.Sum256(b)
		m[filepath.ToSlash(rel)] = hex.EncodeToString(s[:])
		return nil
	})
	return m
}

func assertAllCiphertext(t *testing.T, root string, encryptPaths bool) {
	t.Helper()
	for rel := range snapshot(t, root) {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		if !bytes.HasPrefix(b, []byte("age-encryption.org/v1\n")) {
			t.Errorf("%s is not age ciphertext", rel)
		}
		if bytes.Contains(b, []byte("Ciaran")) || bytes.Contains(b, []byte("secret memory")) {
			t.Errorf("%s contains plaintext", rel)
		}
		if encryptPaths && (strings.Contains(rel, "USER") || strings.Contains(rel, "tax-return") || strings.Contains(rel, "mnemosyne")) {
			t.Errorf("repo path %s reveals a source path", rel)
		}
	}
}

func assertTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	count := 0
	filepath.WalkDir(want, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(want, p)
		gp := filepath.Join(got, rel)
		if d.Type()&fs.ModeSymlink != 0 {
			wt, _ := os.Readlink(p)
			gt, err := os.Readlink(gp)
			if err != nil || gt != wt {
				t.Errorf("symlink %s: got %q, %v; want %q", rel, gt, err, wt)
			}
			count++
			return nil
		}
		wb, _ := os.ReadFile(p)
		gb, err := os.ReadFile(gp)
		if err != nil || !bytes.Equal(wb, gb) {
			t.Errorf("%s differs after restore (%v)", rel, err)
		}
		fi, _ := os.Stat(gp)
		if fi != nil && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s restored with mode %v, want owner-only", rel, fi.Mode().Perm())
		}
		count++
		return nil
	})
	if count == 0 {
		t.Fatal("nothing compared")
	}
}

func TestRoundTrip(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			f := newFixture(t, encryptPaths)
			res := f.seal(false)
			if res.Files != 5 || res.Symlinks != 1 || res.Encrypted != 5 || !res.IndexNew {
				t.Fatalf("first seal: %+v", res)
			}
			assertAllCiphertext(t, f.root, encryptPaths)
			if !encryptPaths {
				if _, err := os.Stat(filepath.Join(f.root, "files/memories/USER.md.age")); err != nil {
					t.Fatalf("plain-paths layout: %v", err)
				}
			}
			dest := filepath.Join(t.TempDir(), "restored")
			rr, err := Restore(f.root, f.ids(), dest, RestoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if rr.Files != 5 || rr.Symlinks != 1 {
				t.Fatalf("restore: %+v", rr)
			}
			assertTreesEqual(t, f.src, dest)
		})
	}
}

func TestUnchangedSnapshotChangesNothing(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	before := snapshot(t, f.root)
	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 5 || res.IndexNew || len(res.Removed) != 0 {
		t.Fatalf("second seal: %+v", res)
	}
	after := snapshot(t, f.root)
	if len(before) != len(after) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed although the source did not", k)
		}
	}
}

func TestChangedAndRemovedFiles(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	f.write("memories/USER.md", "The user moved to Cork.\n")
	os.Remove(filepath.Join(f.src, "SOUL.md"))
	res := f.seal(false)
	if res.Encrypted != 1 || res.Reused != 3 || !res.IndexNew {
		t.Fatalf("seal after change: %+v", res)
	}
	// The old USER.md object and the SOUL.md object are gone.
	if len(res.Removed) != 2 {
		t.Fatalf("removed = %v, want 2 objects", res.Removed)
	}
	dest := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dest, "memories/USER.md"))
	if string(b) != "The user moved to Cork.\n" {
		t.Fatalf("USER.md = %q", b)
	}
}

func TestCacheOrObjectLoss(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)

	// A deleted object (e.g. git reset in a backup script) is re-encrypted.
	var obj string
	for rel := range snapshot(t, f.root) {
		if strings.HasPrefix(rel, "objects/") {
			obj = rel
			break
		}
	}
	os.Remove(filepath.Join(f.root, obj))
	if res := f.seal(false); res.Encrypted != 1 {
		t.Fatalf("after object loss: %+v", res)
	}

	// A lost cache re-encrypts everything once.
	os.RemoveAll(f.cache)
	if res := f.seal(false); res.Encrypted != 5 || len(res.Removed) != 5 {
		t.Fatalf("after cache loss: %+v", res)
	}
	if res := f.seal(false); res.Encrypted != 0 {
		t.Fatalf("after cache rebuilt: %+v", res)
	}
	if _, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPrune(t *testing.T) {
	f := newFixture(t, true)
	stray := filepath.Join(f.root, "memories")
	os.MkdirAll(stray, 0o755)
	os.WriteFile(filepath.Join(stray, "USER.md"), []byte("old plaintext"), 0o644)
	os.WriteFile(filepath.Join(f.root, "README.md"), []byte("# backups"), 0o644)

	f.seal(false)
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("unmanaged file removed without --prune")
	}
	res := f.seal(true)
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("unmanaged file kept with --prune")
	}
	if _, err := os.Stat(filepath.Join(f.root, "README.md")); err != nil {
		t.Fatal("public README removed by --prune")
	}
	if _, err := os.Stat(filepath.Join(f.root, repo.FormatFile)); err != nil {
		t.Fatal(".salt removed by --prune")
	}
	if len(res.Removed) != 1 || res.Removed[0] != "memories" {
		t.Fatalf("removed = %v", res.Removed)
	}
}

func TestSealRefusesOverlap(t *testing.T) {
	f := newFixture(t, true)
	if _, err := Seal(f.root, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Fatal("sealed a repo into itself")
	}
	inner := filepath.Join(f.src, "backup")
	os.MkdirAll(inner, 0o755)
	f.repo.Root = inner
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Fatal("sealed into a repo inside the source")
	}
}

func TestRestoreSafety(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)

	t.Run("wrong key", func(t *testing.T) {
		other, _ := age.GenerateX25519Identity()
		dest := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(f.root, []age.Identity{other}, dest, RestoreOptions{}); err == nil {
			t.Fatal("restored with the wrong key")
		}
	})

	t.Run("non-empty destination", func(t *testing.T) {
		dest := t.TempDir()
		os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("mine"), 0o644)
		if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err == nil {
			t.Fatal("restored over a non-empty directory without --force")
		}
		rr, err := Restore(f.root, f.ids(), dest, RestoreOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(rr.MovedAside, "keep.txt")); err != nil || string(b) != "mine" {
			t.Fatalf("existing directory not preserved: %v", err)
		}
	})

	t.Run("subset", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "r")
		rr, err := Restore(f.root, f.ids(), dest, RestoreOptions{Paths: []string{"memories"}})
		if err != nil || rr.Files != 2 {
			t.Fatalf("subset restore: %+v, %v", rr, err)
		}
		if _, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{Paths: []string{"nope"}}); err == nil {
			t.Fatal("unknown path accepted")
		}
	})

	t.Run("tampered object", func(t *testing.T) {
		var obj string
		for rel := range snapshot(t, f.root) {
			if strings.HasPrefix(rel, "objects/") {
				obj = rel
				break
			}
		}
		p := filepath.Join(f.root, obj)
		b, _ := os.ReadFile(p)
		orig := append([]byte{}, b...)
		b[len(b)-10] ^= 0xff
		os.WriteFile(p, b, 0o644)
		defer os.WriteFile(p, orig, 0o644)
		dest := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err == nil {
			t.Fatal("restored a tampered object")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("failed restore left a destination behind")
		}
	})
}

func TestReadIndexRejectsUnsafePaths(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		f := newFixture(t, true)
		ix := &Index{Version: repo.FormatVersion, Entries: []Entry{{Path: bad, Object: "objects/aa/bb.age"}}}
		b, _, _ := ix.marshal()
		if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadIndex(f.root, f.ids()); err == nil {
			t.Errorf("index path %q accepted", bad)
		}
	}
}

// A large file must stream: sealing and restoring it should allocate far
// less than its size.
func TestLargeFileStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 64 MiB file")
	}
	const size = 64 << 20
	f := newFixture(t, true)
	big, err := os.Create(filepath.Join(f.src, "big.db"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := 0; i < size/len(chunk); i++ {
		for j := range chunk {
			chunk[j] = byte(i*31 + j*7 + j/13)
		}
		big.Write(chunk)
	}
	big.Close()

	alloc := func(fn func()) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		fn()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	const budget = size / 2
	if n := alloc(func() { f.seal(false) }); n > budget {
		t.Errorf("seal allocated %d MiB for a %d MiB file", n>>20, size>>20)
	}
	dest := filepath.Join(t.TempDir(), "r")
	if n := alloc(func() {
		if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err != nil {
			t.Fatal(err)
		}
	}); n > budget {
		t.Errorf("restore allocated %d MiB for a %d MiB file", n>>20, size>>20)
	}
	a, _ := os.Open(filepath.Join(f.src, "big.db"))
	b, _ := os.Open(filepath.Join(dest, "big.db"))
	defer a.Close()
	defer b.Close()
	ha, hb := sha256.New(), sha256.New()
	io.Copy(ha, a)
	io.Copy(hb, b)
	if !bytes.Equal(ha.Sum(nil), hb.Sum(nil)) {
		t.Fatal("large file differs after restore")
	}
}
