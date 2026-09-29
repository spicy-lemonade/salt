package seal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
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

func TestVerify(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	res, err := Verify(f.root, f.ids(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 5 || res.Symlinks != 1 || len(res.Problems) != 0 || len(res.Unreferenced) != 0 {
		t.Fatalf("healthy verify: %+v", res)
	}

	var objs []string
	for rel := range snapshot(t, f.root) {
		if strings.HasPrefix(rel, "objects/") {
			objs = append(objs, rel)
		}
	}
	// Tamper with one object, delete another, add a stray one.
	p := filepath.Join(f.root, objs[0])
	b, _ := os.ReadFile(p)
	b[len(b)-5] ^= 0xff
	os.WriteFile(p, b, 0o644)
	os.Remove(filepath.Join(f.root, objs[1]))
	stray := filepath.Join(f.root, "objects", "zz", "stray.age")
	os.MkdirAll(filepath.Dir(stray), 0o755)
	os.WriteFile(stray, []byte("age-encryption.org/v1\n"), 0o644)

	res, err = Verify(f.root, f.ids(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 2 {
		t.Fatalf("problems = %v, want 2", res.Problems)
	}
	joined := strings.Join(res.Problems, "\n")
	if !strings.Contains(joined, "is missing") || !strings.Contains(joined, "cannot be decrypted") {
		t.Fatalf("problems = %v", res.Problems)
	}
	if len(res.Unreferenced) != 1 || res.Unreferenced[0] != "objects/zz/stray.age" {
		t.Fatalf("unreferenced = %v", res.Unreferenced)
	}

	other, _ := age.GenerateX25519Identity()
	if _, err := Verify(f.root, []age.Identity{other}, 0); err == nil {
		t.Fatal("verify with the wrong key succeeded")
	}
}

func TestSealErrors(t *testing.T) {
	f := newFixture(t, true)
	if _, err := Seal(f.src, f.repo, Options{}); err == nil {
		t.Error("Seal without a cache dir succeeded")
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := Seal(file, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Error("Seal of a file (not a dir) succeeded")
	}
	if _, err := Seal(filepath.Join(t.TempDir(), "missing"), f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Error("Seal of a missing dir succeeded")
	}
	// The cache dir cannot be created (its parent is a file).
	if _, err := Seal(f.src, f.repo, Options{CacheDir: filepath.Join(file, "cache")}); err == nil {
		t.Error("Seal with an unwritable cache succeeded")
	}
}

func TestSealSkipsSpecialFilesAndExcludes(t *testing.T) {
	f := newFixture(t, true)
	if err := syscall.Mkfifo(filepath.Join(f.src, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	os.MkdirAll(filepath.Join(f.src, ".git"), 0o755)
	os.WriteFile(filepath.Join(f.src, ".git", "config"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(f.src, ".DS_Store"), []byte("x"), 0o644)
	res, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "pipe" || res.Files != 5 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRestoreRefusesSymlinkParent(t *testing.T) {
	f := newFixture(t, true)
	outside := t.TempDir()
	ix := &Index{Version: repo.FormatVersion, Entries: []Entry{
		{Path: "x", Symlink: outside},
		{Path: "x/y", Symlink: "z"},
	}}
	b, _, _ := ix.marshal()
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("restore through a symlink: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("restore wrote outside the destination")
	}
}

func TestRestoreEdgeCases(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	// An existing empty directory is fine and gets replaced.
	empty := t.TempDir()
	if _, err := Restore(f.root, f.ids(), empty, RestoreOptions{}); err != nil {
		t.Fatalf("restore into an empty dir: %v", err)
	}
	// A destination that is a file counts as non-empty.
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := Restore(f.root, f.ids(), file, RestoreOptions{}); err == nil {
		t.Fatal("restore over a file succeeded")
	}
	// A missing index.
	os.Remove(filepath.Join(f.root, repo.IndexFile))
	if _, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{}); err == nil {
		t.Fatal("restore without an index succeeded")
	}
	if _, err := Verify(f.root, f.ids(), 0); err == nil {
		t.Fatal("verify without an index succeeded")
	}
}

func TestReadIndexRejectsBadIndexes(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     "{",
		"old version":  `{"version": 99, "entries": []}`,
		"bad object":   `{"version": 1, "entries": [{"path": "a", "object": "../x"}]}`,
		"empty object": `{"version": 1, "entries": [{"path": "a"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			if err := writeIndex(f.root, []byte(body), f.repo.Recipients); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadIndex(f.root, f.ids()); err == nil {
				t.Fatalf("index %q accepted", body)
			}
		})
	}
}

func TestEncryptToErrors(t *testing.T) {
	f := newFixture(t, true)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644)
	rt := openRoot(t, dir)
	if _, err := encryptTo(rt, "file/sub/obj.age", strings.NewReader("x"), f.repo.Recipients); err == nil {
		t.Error("encryptTo under a file succeeded")
	}
	if _, err := encryptTo(rt, "obj.age", errReader{}, f.repo.Recipients); err == nil {
		t.Error("encryptTo with a failing reader succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a failed encryptTo left files behind: %v", entries)
	}
	if _, _, err := hashFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("hashFile of a missing file succeeded")
	}
	if d, err := DefaultCacheDir(); err != nil || !strings.HasSuffix(d, "salt") {
		t.Errorf("DefaultCacheDir = %q, %v", d, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSealAndRestoreFileErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read unreadable files")
	}
	f := newFixture(t, true)
	locked := filepath.Join(f.src, "locked.md")
	os.WriteFile(locked, []byte("x"), 0o000)
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Fatal("Seal of an unreadable file succeeded")
	}
	os.Remove(locked)

	// Leftover temp files from an interrupted seal are cleaned up.
	os.WriteFile(filepath.Join(f.root, ".salt-tmp-1"), []byte("x"), 0o644)
	res := f.seal(false)
	if !slices.Contains(res.Removed, ".salt-tmp-1") {
		t.Fatalf("removed = %v", res.Removed)
	}

	// A missing object makes restore fail and leave nothing behind.
	for rel := range snapshot(t, f.root) {
		if strings.HasPrefix(rel, "objects/") {
			os.Remove(filepath.Join(f.root, rel))
			break
		}
	}
	dest := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err == nil {
		t.Fatal("restore with a missing object succeeded")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed restore left a destination")
	}
}

func TestRestoreSizeMismatch(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	ix, err := ReadIndex(f.root, f.ids())
	if err != nil {
		t.Fatal(err)
	}
	for i := range ix.Entries {
		if ix.Entries[i].Symlink == "" {
			ix.Entries[i].Size++ // index no longer matches the content
			break
		}
	}
	b, _, _ := ix.marshal()
	writeIndex(f.root, b, f.repo.Recipients)
	_, err = Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not match the index") {
		t.Fatalf("restore with a wrong size: %v", err)
	}
}

// writeBombIndex streams a crafted index into the repo: small once
// compressed, huge once decompressed.
func writeBombIndex(t *testing.T, f *fixture, gen func(w io.Writer)) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		gen(pw)
		pw.Close()
	}()
	if _, err := encryptTo(openRoot(t, f.root), repo.IndexFile, pr, f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(f.root, repo.IndexFile)); fi.Size() > 2<<20 {
		t.Fatalf("bomb index is %d bytes; expected it to compress well", fi.Size())
	}
}

func allocDuring(fn func()) uint64 {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// A tampered index must be refused without using lots of memory.
func TestIndexBombs(t *testing.T) {
	if testing.Short() {
		t.Skip("decompresses tens of MB")
	}
	const budget = 200 << 20
	tests := []struct {
		name string
		gen  func(w io.Writer)
		want string
	}{
		{"a million entries", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,"entries":[`)
			for i := 0; i < 1_000_000; i++ {
				if i > 0 {
					bw.WriteByte(',')
				}
				bw.WriteString(`{"path":"a","object":"objects/aa/b.age","size":1,"mode":420}`)
			}
			bw.WriteString(`]}`)
			bw.Flush()
		}, "more than 100000 entries"},
		{"one huge path", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,"entries":[{"path":"`)
			chunk := bytes.Repeat([]byte("a"), 1<<20)
			for i := 0; i < 100; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`"}]}`)
			bw.Flush()
		}, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, true)
			writeBombIndex(t, f, tt.gen)
			var err error
			used := allocDuring(func() { _, err = ReadIndex(f.root, f.ids()) })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadIndex error = %v, want %q", err, tt.want)
			}
			t.Logf("allocated %d MiB", used>>20)
			if used > budget {
				t.Fatalf("ReadIndex allocated %d MiB, budget %d MiB", used>>20, budget>>20)
			}
		})
	}
}

func TestSealRefusesTooManyFiles(t *testing.T) {
	f := newFixture(t, true)
	old := MaxIndexEntriesForTest(3)
	defer MaxIndexEntriesForTest(old)
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("Seal with too many files: %v", err)
	}
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	rt, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	return rt
}

// Someone with push access commits objects/ (or files/) as a symlink to a
// folder outside the repo. Seal must refuse rather than write there.
func TestSealRefusesSymlinkedObjectFolders(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		dir := map[bool]string{true: repo.ObjectsDir, false: repo.FilesDir}[encryptPaths]
		t.Run(dir, func(t *testing.T) {
			f := newFixture(t, encryptPaths)
			outside := t.TempDir()
			// With visible paths the object name is predictable, so a file
			// outside could be overwritten.
			victim := filepath.Join(outside, "SOUL.md.age")
			os.WriteFile(victim, []byte("do not touch"), 0o644)
			if err := os.Symlink(outside, filepath.Join(f.root, dir)); err != nil {
				t.Fatal(err)
			}
			if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil {
				t.Fatal("Seal wrote through a symlinked folder")
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 1 {
				t.Fatalf("files written outside the repo: %v", entries)
			}
			if b, _ := os.ReadFile(victim); string(b) != "do not touch" {
				t.Fatal("a file outside the repo was overwritten")
			}
		})
	}
}

// A symlinked subfolder inside objects/ is refused the same way.
func TestSealRefusesSymlinkInsideObjects(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	outside := t.TempDir()
	// Point every possible two-letter shard at the outside folder.
	os.RemoveAll(filepath.Join(f.root, repo.ObjectsDir))
	os.MkdirAll(filepath.Join(f.root, repo.ObjectsDir), 0o755)
	for i := 0; i < 256; i++ {
		os.Symlink(outside, filepath.Join(f.root, repo.ObjectsDir, fmt.Sprintf("%02x", i)))
	}
	os.RemoveAll(f.cache)
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Fatal("Seal wrote through a symlinked shard folder")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("files written outside the repo: %v", entries)
	}
}

// Restore must not read objects through a symlink that leaves the repo.
func TestRestoreRefusesSymlinkedObjects(t *testing.T) {
	f := newFixture(t, false)
	f.seal(false)
	moved := t.TempDir()
	files := filepath.Join(f.root, repo.FilesDir)
	if err := os.Rename(files, filepath.Join(moved, "files")); err != nil {
		t.Fatal(err)
	}
	os.Symlink(filepath.Join(moved, "files"), files)
	if _, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{}); err == nil {
		t.Fatal("restore read objects from outside the repo")
	}
	if _, err := Verify(f.root, f.ids(), 0); !errors.Is(err, ErrForeignSymlink) {
		t.Fatalf("verify with a symlinked files/: %v", err)
	}
}

// A symlink pointing back inside the repo (objects/ -> .) once made seal's
// clean-up delete files from .git and .salt while reporting success.
func TestSymlinkPointingInsideTheRepo(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	os.MkdirAll(filepath.Join(f.root, ".git"), 0o755)
	os.WriteFile(filepath.Join(f.root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.RemoveAll(filepath.Join(f.root, repo.ObjectsDir))
	os.Symlink(".", filepath.Join(f.root, repo.ObjectsDir))

	_, err := Seal(f.src, f.repo, Options{CacheDir: f.cache})
	if !errors.Is(err, ErrForeignSymlink) || !strings.Contains(err.Error(), "at objects") {
		t.Fatalf("seal: %v", err)
	}
	for _, p := range []string{".git/HEAD", repo.FormatFile, repo.RecipientsFile, repo.IndexFile} {
		if _, err := os.Stat(filepath.Join(f.root, p)); err != nil {
			t.Errorf("%s was deleted", p)
		}
	}
	if _, err := Restore(f.root, f.ids(), filepath.Join(t.TempDir(), "r"), RestoreOptions{}); !errors.Is(err, ErrForeignSymlink) {
		t.Errorf("restore: %v", err)
	}
	if _, err := Verify(f.root, f.ids(), 0); !errors.Is(err, ErrForeignSymlink) {
		t.Errorf("verify: %v", err)
	}
}

// Symlinks anywhere salt keeps data are refused, not just objects/.
func TestForeignSymlinksAnywhereManaged(t *testing.T) {
	for _, link := range []string{repo.IndexFile, repo.Dir + "/extra", repo.FilesDir, repo.ObjectsDir + "/ab"} {
		t.Run(link, func(t *testing.T) {
			f := newFixture(t, true)
			f.seal(false)
			p := filepath.Join(f.root, filepath.FromSlash(link))
			os.RemoveAll(p)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(t.TempDir(), p); err != nil {
				t.Fatal(err)
			}
			if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); !errors.Is(err, ErrForeignSymlink) {
				t.Fatalf("seal with a symlink at %s: %v", link, err)
			}
		})
	}
}
