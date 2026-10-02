package seal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
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
	f.write("data/memory.db", "SQLite format 3\x00"+strings.Repeat("\x01\x02", 5000))
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
		if encryptPaths && (strings.Contains(rel, "USER") || strings.Contains(rel, "tax-return") || strings.Contains(rel, "memory.db")) {
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
		if wi, _ := os.Stat(p); fi != nil && wi != nil && !fi.ModTime().Equal(wi.ModTime()) {
			t.Errorf("%s restored with last-modified %v, want %v", rel, fi.ModTime(), wi.ModTime())
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

// validEntry names the path in its errors, but a path can be up to 4096
// bytes, so each message shortens it and still starts readably.
func TestValidEntryClipsPaths(t *testing.T) {
	long := strings.Repeat("a", maxIndexString)
	hash := strings.Repeat("0", 64)
	for name, tt := range map[string]struct {
		e      Entry
		prefix string
	}{
		"hash":           {Entry{Path: long, Object: "objects/aa/b.age"}, `hash for "aaaa`},
		"size":           {Entry{Path: long, Object: "objects/aa/b.age", SHA256: hash, Size: -1}, `size for "aaaa`},
		"symlink":        {Entry{Path: long, Symlink: "b", SHA256: hash}, `symlink "aaaa`},
		"object":         {Entry{Path: long, Object: "/" + long[1:], SHA256: hash}, `object for "aaaa`},
		"unsafe path":    {Entry{Path: "/" + long[1:], Object: "objects/aa/b.age", SHA256: hash}, `unsafe path "/aaa`},
		"short unsafe":   {Entry{Path: "../x"}, `unsafe path "../x"`},
		"empty path":     {Entry{Path: ""}, `unsafe path ""`},
		"short readable": {Entry{Path: "a", Object: "objects/aa/b.age"}, `hash for a is not 64 hex characters`},
	} {
		t.Run(name, func(t *testing.T) {
			err := validEntry(&tt.e)
			if err == nil || !strings.HasPrefix(err.Error(), tt.prefix) {
				t.Fatalf("validEntry error = %.300v, want it to start with %q", err, tt.prefix)
			}
			if len(err.Error()) > 200 {
				t.Fatalf("error message is %d bytes long", len(err.Error()))
			}
		})
	}
}

// A path of exactly maxIndexString bytes that json.Marshal escapes to six
// times its length is exactly at the raw string cap, and must still be read.
func TestReadIndexAcceptsLongEscapedPath(t *testing.T) {
	f := newFixture(t, true)
	p := strings.Repeat("&", maxIndexString)
	ix := &Index{Version: repo.FormatVersion, Entries: []Entry{{Path: p, Object: "objects/aa/b.age", SHA256: strings.Repeat("0", 64)}}}
	b, _, err := ix.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(strings.Repeat(`\u0026`, maxIndexString))) {
		t.Fatal("json.Marshal no longer escapes &; this test needs another character")
	}
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	got, err := ReadIndex(f.root, f.ids())
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Path != p {
		t.Fatal("path at the limit did not come back unchanged")
	}
}

func TestCheckIndexEntries(t *testing.T) {
	hash := strings.Repeat("0", 64)
	long := strings.Repeat("a", maxIndexString+1)
	for name, tt := range map[string]struct {
		e    Entry
		want string
	}{
		"path at the limit":          {Entry{Path: strings.Repeat("a", maxIndexString), Object: "objects/aa/b.age", SHA256: hash}, ""},
		"symlink":                    {Entry{Path: "a", Symlink: "b"}, ""},
		"path too long":              {Entry{Path: long, Object: "objects/aa/b.age", SHA256: hash}, "entry longer than 4096 bytes"},
		"symlink target too long":    {Entry{Path: "a", Symlink: long}, "entry longer than 4096 bytes"},
		"plain object name too long": {Entry{Path: strings.Repeat("a", maxIndexString-5), Object: path.Join(repo.FilesDir, strings.Repeat("a", maxIndexString-5)) + ".age", SHA256: hash}, "entry longer than 4096 bytes"},
		"unsafe path":                {Entry{Path: "../x", Object: "objects/aa/b.age", SHA256: hash}, "unsafe path"},
		"bad hash":                   {Entry{Path: "a", Object: "objects/aa/b.age", SHA256: "ABC"}, "not 64 hex characters"},
	} {
		t.Run(name, func(t *testing.T) {
			entries := []Entry{{Path: "ok", Object: "objects/aa/c.age", SHA256: hash}, tt.e}
			err := checkIndexEntries(entries)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("checkIndexEntries: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("checkIndexEntries error = %v, want %q", err, tt.want)
			}
			if len(err.Error()) > 200 {
				t.Fatalf("error message is %d bytes long", len(err.Error()))
			}
		})
	}
}

// Seal must refuse an entry that ReadIndex would refuse, before it writes an
// index. The hook stands in for a path too long to create on this system.
func TestSealRefusesEntriesRestoreWouldRefuse(t *testing.T) {
	f := newFixture(t, true)
	t.Cleanup(func() { entriesHook = nil })
	breakEntries := func(entries []Entry) []Entry {
		entries[0].Path = strings.Repeat("a", maxIndexString+1)
		return entries
	}
	indexPath := filepath.Join(f.root, repo.IndexFile)

	entriesHook = breakEntries
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil || !strings.Contains(err.Error(), "entry longer than 4096 bytes") {
		t.Fatalf("Seal error = %v, want entry longer than 4096 bytes", err)
	}
	if _, err := os.Lstat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("index.age written despite a bad entry: %v", err)
	}

	// A failed seal must also leave an existing index alone.
	entriesHook = nil
	f.seal(false)
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	f.write("SOUL.md", "be kinder\n")
	entriesHook = breakEntries
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil {
		t.Fatal("Seal accepted a bad entry")
	}
	if after, _ := os.ReadFile(indexPath); !bytes.Equal(before, after) {
		t.Fatal("a failed seal replaced the index")
	}
}

// Seal must refuse an index larger than ReadIndex accepts, before it writes
// anything. Every entry is valid on its own; only the total is too big.
func TestSealRefusesOversizedIndex(t *testing.T) {
	f := newFixture(t, true)
	t.Cleanup(func() { entriesHook = nil })
	// Each path escapes to 6 times its length, so few entries are needed.
	big := Entry{Path: strings.Repeat("&", maxIndexString), Object: "objects/aa/b.age", SHA256: strings.Repeat("0", 64)}
	entriesHook = func(entries []Entry) []Entry {
		for len(entries)*maxIndexJSONString <= maxIndexSize {
			entries = append(entries, big)
		}
		return entries
	}
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache}); err == nil || !strings.Contains(err.Error(), "salt supports at most 33554432") {
		t.Fatalf("Seal error = %v, want the index size limit", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, repo.IndexFile)); !os.IsNotExist(err) {
		t.Fatalf("index.age written despite being too large: %v", err)
	}
}

// A field name or value under the token caps can still be tens of KB, so
// salt's own messages about unexpected tokens must shorten it.
func TestReadIndexRejectsUnexpectedTokens(t *testing.T) {
	long := strings.Repeat("k", 20_000)
	for name, tt := range map[string]struct{ body, want string }{
		"unknown field":        {`{"version":1,"extra":1}`, `unexpected field extra`},
		"long unknown field":   {`{"version":1,"` + long + `":1}`, `unexpected field "kkkk`},
		"string for the list":  {`{"version":1,"entries":"` + long + `"}`, `expected "[", got "kkkk`},
		"object for the list":  {`{"version":1,"entries":{}}`, `expected "[", got {`},
		"list for the index":   {`[]`, `expected "{", got [`},
		"number for the index": {`1`, `expected "{", got 1`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			if err := writeIndex(f.root, []byte(tt.body), f.repo.Recipients); err != nil {
				t.Fatal(err)
			}
			_, err := ReadIndex(f.root, f.ids())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadIndex error = %.300v, want %q", err, tt.want)
			}
			if len(err.Error()) > 200 {
				t.Fatalf("error message is %d bytes long", len(err.Error()))
			}
		})
	}
}

// decodeErr shortens what encoding/json says, and leaves salt's own messages
// alone however long they are.
func TestDecodeErrors(t *testing.T) {
	decode := func(body string) error {
		_, err := decodeIndex(json.NewDecoder(strings.NewReader(body)))
		return err
	}

	err := decode(`{"version":` + strings.Repeat("9", 1000) + `}`)
	if err == nil || !strings.Contains(err.Error(), "cannot unmarshal number") || !strings.HasSuffix(err.Error(), " bytes)") {
		t.Fatalf("decoder error = %.300v, want it shortened", err)
	}
	if len(err.Error()) > 200 {
		t.Fatalf("decoder error is %d bytes long", len(err.Error()))
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatal("shortened error lost the original")
	}

	// A salt message from inside the decode path comes through exactly.
	p := strings.Repeat("a", 200)
	err = decode(`{"version":1,"entries":[{"path":"` + p + `","object":"objects/aa/b.age","sha256":"abc"}]}`)
	if want := "hash for " + clip(p) + " is not 64 hex characters"; err == nil || err.Error() != want {
		t.Fatalf("salt error = %v, want %q unchanged", err, want)
	}

	// decodeErr only runs where the decoder's errors come out, and salt's
	// own messages never go there. Show that one would not be clipped
	// anyway if it were long.
	own := fmt.Errorf("%w at %s", ErrForeignSymlink, strings.Repeat("b", 300))
	if got := decodeErr(own); got != own || len(got.Error()) <= maxDecodeErr {
		t.Fatalf("decodeErr changed a salt error: %v", got)
	}

	// A short decoder error is not wrapped.
	err = decode(`{"version":"x"}`)
	if _, clipped := err.(*clippedError); err == nil || clipped {
		t.Fatalf("short decoder error = %#v, want it passed on as is", err)
	}
	if decodeErr(nil) != nil {
		t.Fatal("nil error became non-nil")
	}
}

// removeStale deletes files under objects/, so it must not walk into a
// symlinked objects/ even if it is ever called without CheckNoSymlinks first.
func TestRemoveStaleRefusesSymlinkedFolder(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprintf("prune=%v", prune), func(t *testing.T) {
			f := newFixture(t, true)
			git := filepath.Join(f.root, ".git")
			os.MkdirAll(filepath.Join(git, "refs", "heads"), 0o755)
			os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
			os.WriteFile(filepath.Join(git, "refs", "heads", "main"), []byte("0123\n"), 0o644)
			if err := os.Symlink(".git", filepath.Join(f.root, repo.ObjectsDir)); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, git)

			removed, err := removeStale(openRoot(t, f.root), map[string]bool{}, prune)
			if !errors.Is(err, ErrForeignSymlink) || !strings.Contains(err.Error(), "at objects") {
				t.Fatalf("removeStale error = %v, want a foreign symlink at objects", err)
			}
			if len(removed) != 0 {
				t.Fatalf("removeStale removed %v", removed)
			}
			if after := snapshot(t, git); !maps.Equal(before, after) {
				t.Fatalf(".git changed: before %v, after %v", before, after)
			}
		})
	}
}

func TestWalkRepoDir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "objects", "aa"), 0o755)
	os.WriteFile(filepath.Join(root, "objects", "aa", "b.age"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "elsewhere"), 0o755)
	os.WriteFile(filepath.Join(root, "elsewhere", "c.age"), []byte("x"), 0o644)
	os.Symlink("elsewhere", filepath.Join(root, "files"))
	rt := openRoot(t, root)

	walk := func(dir string) ([]string, error) {
		var seen []string
		err := walkRepoDir(rt, dir, func(rel string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				seen = append(seen, rel)
			}
			return err
		})
		return seen, err
	}
	if seen, err := walk("objects"); err != nil || !slices.Equal(seen, []string{"objects/aa/b.age"}) {
		t.Fatalf("real folder: %v, %v", seen, err)
	}
	if seen, err := walk("missing"); err != nil || len(seen) != 0 {
		t.Fatalf("missing folder: %v, %v", seen, err)
	}
	if seen, err := walk("files"); !errors.Is(err, ErrForeignSymlink) || len(seen) != 0 {
		t.Fatalf("symlinked folder: %v, %v", seen, err)
	}
	os.Chmod(root, 0o000)
	defer os.Chmod(root, 0o755)
	if _, err := walk("objects"); err == nil || errors.Is(err, ErrForeignSymlink) {
		t.Fatalf("unreadable repo: %v", err)
	}
}

// An interrupted restore removes its partly restored files, never touches
// dest, and reports its temporary folder gone exactly once.
func TestRestoreCancelledCleansUp(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var tracked string
	doneCalls := 0
	track := func(tmp string) func() {
		tracked = tmp
		cancel() // interrupted as soon as the restore starts writing
		return func() { doneCalls++ }
	}
	_, err := Restore(f.root, f.ids(), dest, RestoreOptions{Context: ctx, Track: track})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Restore error = %v, want context.Canceled", err)
	}
	if tracked == "" || filepath.Dir(tracked) != parent {
		t.Fatalf("tracked %q, want a folder in %s", tracked, parent)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if doneCalls != 1 {
		t.Fatalf("done called %d times, want 1", doneCalls)
	}
}

func TestRestoreTracksUntilInPlace(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	dest := filepath.Join(t.TempDir(), "out")
	var tracked string
	doneCalls := 0
	track := func(tmp string) func() {
		tracked = tmp
		return func() { doneCalls++ }
	}
	if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{Track: track}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tracked); !os.IsNotExist(err) || doneCalls != 1 {
		t.Fatalf("temporary folder %s: %v, done called %d times", tracked, err, doneCalls)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "SOUL.md")); string(b) != "be kind\n" {
		t.Fatalf("restored SOUL.md = %q", b)
	}
}

func TestCtxReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := ctxReader{ctx, strings.NewReader("abcdef")}
	buf := make([]byte, 3)
	if n, err := r.Read(buf); n != 3 || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	cancel()
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after cancel = %d, %v", n, err)
	}
}

// A tampered index naming many missing files with long paths must not
// flood the output: Verify counts every problem but describes at most
// MaxProblems, each with its path shortened.
func TestVerifyCapsProblems(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	// Written in reverse, so the order of the list comes from the paths.
	ix := &Index{Version: repo.FormatVersion}
	for i := 199; i >= 0; i-- {
		p := fmt.Sprintf("%03d/%s", i, strings.Repeat("a", maxIndexString-4))
		ix.Entries = append(ix.Entries, Entry{Path: p, Object: fmt.Sprintf("objects/aa/missing%d.age", i), SHA256: strings.Repeat("0", 64), Size: 1})
	}
	b, _, err := ix.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(f.root, f.ids(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProblemCount != 200 || len(res.Problems) != MaxProblems {
		t.Fatalf("ProblemCount = %d, %d described; want 200 and %d", res.ProblemCount, len(res.Problems), MaxProblems)
	}
	for i, p := range res.Problems {
		if len(p) > 200 || !strings.Contains(p, "is missing") {
			t.Fatalf("problem is %d bytes: %.300s", len(p), p)
		}
		// The first 50 by path, in order: 000/… to 049/….
		if want := fmt.Sprintf(`"%03d/aaa`, i); !strings.HasPrefix(p, want) {
			t.Fatalf("problem %d = %.60s…, want it to start with %s", i, p, want)
		}
	}
	// The same list on every run, whatever order the workers finish in.
	for run := 0; run < 3; run++ {
		again, err := Verify(f.root, f.ids(), 4)
		if err != nil || !slices.Equal(again.Problems, res.Problems) {
			t.Fatalf("run %d gave a different list: %v", run, err)
		}
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
				bw.WriteString(`{"path":"a","object":"objects/aa/b.age","sha256":"` + strings.Repeat("0", 64) + `","size":1,"mode":420}`)
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
		}, "string longer than 24576 bytes"},
		// Whitespace is not a string, so only the total size cap stops it.
		{"padding past the size cap", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,`)
			chunk := bytes.Repeat([]byte(" "), 1<<20)
			for i := 0; i < 33; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`"entries":[]}`)
			bw.Flush()
		}, "larger than"},
		{"huge field name", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"`)
			chunk := bytes.Repeat([]byte("k"), 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`":1}`)
			bw.Flush()
		}, "string longer than 24576 bytes"},
		// encoding/json turns each invalid UTF-8 byte into a 3-byte U+FFFD,
		// so this is the worst case for the field name.
		{"huge invalid UTF-8 field name", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"`)
			chunk := bytes.Repeat([]byte{0xff}, 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`":1}`)
			bw.Flush()
		}, "string longer than 24576 bytes"},
		{"huge invalid UTF-8 hash", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,"entries":[{"path":"a","object":"objects/aa/b.age","sha256":"`)
			chunk := bytes.Repeat([]byte{0xff}, 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`"}]}`)
			bw.Flush()
		}, "string longer than 24576 bytes"},
		{"huge size number", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,"entries":[{"path":"a","object":"objects/aa/b.age","sha256":"` + strings.Repeat("0", 64) + `","size":`)
			chunk := bytes.Repeat([]byte("9"), 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`}]}`)
			bw.Flush()
		}, "value longer than 64 bytes"},
		{"huge version number", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1`)
			chunk := bytes.Repeat([]byte("0"), 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`,"entries":[]}`)
			bw.Flush()
		}, "value longer than 64 bytes"},
		{"huge value where the list should be", func(w io.Writer) {
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"version":1,"entries":"`)
			chunk := bytes.Repeat([]byte("v"), 1<<20)
			for i := 0; i < 30; i++ {
				bw.Write(chunk)
			}
			bw.WriteString(`"}`)
			bw.Flush()
		}, "string longer than 24576 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, true)
			writeBombIndex(t, f, tt.gen)
			var err error
			used := allocDuring(func() { _, err = ReadIndex(f.root, f.ids()) })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadIndex error = %.200s, want %q", err, tt.want)
			}
			// Nothing from the index is echoed at length into logs.
			if len(err.Error()) > 200 {
				t.Fatalf("error message is %d bytes long", len(err.Error()))
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

func TestIndexEntryChecks(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	const badHash = "is not 64 hex characters"
	file := func(hash string) string {
		return `{"path":"a","object":"objects/aa/b.age","sha256":"` + hash + `","size":1}`
	}
	for name, tt := range map[string]struct{ entry, want string }{
		"missing hash":       {`{"path":"a","object":"objects/aa/b.age","size":1}`, badHash},
		"empty hash":         {file(""), badHash},
		"short hash":         {file("abc"), badHash},
		"63 characters":      {file(sha[:63]), badHash},
		"uppercase hash":     {file(strings.ToUpper(sha)), badHash},
		"not hex":            {file(strings.Repeat("zz", 32)), badHash},
		"bad last character": {file(sha[:63] + "g"), badHash},
		"huge hash":          {file(strings.Repeat("a", 5000)), "entry longer than 4096 bytes"},
		"negative size":      {`{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":-1}`, "size for a is negative"},
		"symlink with hash":  {`{"path":"a","symlink":"b","sha256":"` + sha + `"}`, "must not have an object or hash"},
		"symlink w/ object":  {`{"path":"a","symlink":"b","object":"objects/aa/b.age"}`, "must not have an object or hash"},
		"symlink with date":  {`{"path":"a","symlink":"b","mtime":1}`, "symlink a must not have a last-modified date"},
		"date as string":     {`{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":1,"mtime":"2025-01-01"}`, "cannot unmarshal string"},
		"fractional date":    {`{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":1,"mtime":1.5}`, "cannot unmarshal number 1.5"},
		"date past int64":    {`{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":1,"mtime":9223372036854775808}`, "cannot unmarshal number"},
		"very long date":     {`{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":1,"mtime":` + strings.Repeat("9", 100) + `}`, "value longer than 64 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			body := `{"version":1,"entries":[` + tt.entry + `]}`
			if err := writeIndex(f.root, []byte(body), f.repo.Recipients); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadIndex(f.root, f.ids()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadIndex error = %v, want %q", err, tt.want)
			}
		})
	}
	// A normal file, with or without a date (before 1970 is negative), and a
	// normal symlink are accepted.
	f := newFixture(t, true)
	ok := `{"version":1,"entries":[{"path":"a","object":"objects/aa/b.age","sha256":"` + sha + `","size":1},` +
		`{"path":"b","object":"objects/aa/c.age","sha256":"` + sha + `","size":1,"mtime":-9223372036854775808},` +
		`{"path":"c","object":"objects/aa/d.age","sha256":"` + sha + `","size":1,"mtime":1759233600000000000},` +
		`{"path":"l","symlink":"a"}]}`
	writeIndex(f.root, []byte(ok), f.repo.Recipients)
	if _, err := ReadIndex(f.root, f.ids()); err != nil {
		t.Fatalf("valid index refused: %v", err)
	}
}

func TestClip(t *testing.T) {
	if clip("short") != "short" {
		t.Error("short value changed")
	}
	long := clip(strings.Repeat("é", 100))
	if len(long) > 80 || !strings.Contains(long, "(200 bytes)") {
		t.Errorf("clip = %q", long)
	}
}

// Paths in messages go through Show when set and are unchanged otherwise.
func TestShowPathsInMessages(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "src")
	if err := checkDisjoint(inner, dir, nil); err == nil || !strings.Contains(err.Error(), "source "+inner+" and repository "+dir) {
		t.Fatalf("without Show: %v", err)
	}
	mark := func(p string) string { return "<" + filepath.Base(p) + ">" }
	if err := checkDisjoint(inner, dir, mark); err == nil || !strings.Contains(err.Error(), "source <src> and repository <"+filepath.Base(dir)+">") {
		t.Fatalf("with Show: %v", err)
	}
}
