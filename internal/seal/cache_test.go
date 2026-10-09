package seal

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// CachedPaths lists every file the last seal sealed, and nothing when there
// is no cache or it is damaged.
func TestCachedPaths(t *testing.T) {
	f := newFixture(t, true)
	if got := CachedPaths(f.cache, f.root); got != nil {
		t.Fatalf("before any seal: %v", got)
	}
	f.seal(false)
	want := []string{"NOTES.md", "data/store.db", "notes/facts.md", "notes/profile.md", "projects/tax-return-2026/plan.md"}
	if got := CachedPaths(f.cache, f.root); !slices.Equal(got, want) {
		t.Fatalf("CachedPaths = %v, want %v", got, want)
	}
	_, p := f.loadCache()
	os.WriteFile(p, []byte("{not json"), 0o600)
	if got := CachedPaths(f.cache, f.root); got != nil {
		t.Fatalf("damaged cache: %v", got)
	}
}

// objectOf returns the path in the repo of the object holding rel, as the
// cache records it.
func (f *fixture) objectOf(rel string) string {
	f.t.Helper()
	c, _ := f.loadCache()
	ce, ok := c.Files[rel]
	if !ok {
		f.t.Fatalf("%s is not in the cache", rel)
	}
	return filepath.Join(f.root, filepath.FromSlash(ce.Object))
}

// replaceSameSize writes other bytes of the same size over the file at p,
// dated an hour earlier, as a git pull or reset that changed it would.
func replaceSameSize(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, int(fi.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	earlier := fi.ModTime().Add(-time.Hour)
	if err := os.Chtimes(p, earlier, earlier); err != nil {
		t.Fatal(err)
	}
}

// An object replaced by another of the same size is not reused, so the next
// seal repairs the backup.
func TestSwappedObjectIsSealedAgain(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	replaceSameSize(t, f.objectOf("NOTES.md"))
	if res := f.seal(false); res.Encrypted != 1 || res.Reused != 4 {
		t.Fatalf("seal after the swap: %+v", res)
	}
	if res, err := Verify(f.root, f.ids(), VerifyOptions{}); err != nil || res.ProblemCount != 0 {
		t.Fatalf("verify after the repair: %+v, %v", res, err)
	}
}

// index.age replaced by another file of the same size is written again.
func TestSwappedIndexIsWrittenAgain(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	replaceSameSize(t, filepath.Join(f.root, repo.IndexFile))
	if res := f.seal(false); !res.IndexNew || res.Encrypted != 0 {
		t.Fatalf("seal after the swap: %+v", res)
	}
	if _, err := ReadIndex(f.root, f.ids(), SignatureOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A cache from a salt that recorded no times keeps every object and the
// index, checked by size alone, and gains their times.
func TestCacheWithoutTimes(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	c, p := f.loadCache()
	c.IndexModTime = 0
	for rel, ce := range c.Files {
		parts := ce.all()
		for i := range parts {
			parts[i].ModTime = 0
		}
		c.Files[rel] = newCacheEntry(ce.SHA256, parts)
	}
	if err := c.save(p); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.root)
	if res := f.seal(false); res.Encrypted != 0 || res.Reused != 5 || res.IndexNew {
		t.Fatalf("seal with an older cache: %+v", res)
	}
	if !maps.Equal(before, snapshot(t, f.root)) {
		t.Fatal("the repo changed")
	}
	c, _ = f.loadCache()
	if c.IndexModTime == 0 {
		t.Error("the index's time was not recorded")
	}
	for rel, ce := range c.Files {
		for _, part := range ce.all() {
			if part.ModTime == 0 {
				t.Errorf("%s: no time recorded", rel)
			}
		}
	}
}

func TestWritePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := filepath.Join(dir, "pushed.json")
	// A file at the name an older salt wrote to first is neither written
	// through nor removed.
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("keep"), 0o600)
	os.MkdirAll(dir, 0o700)
	if err := os.Symlink(outside, p+".tmp"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		if err := WritePrivate(p, []byte(want)); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Fatalf("read back %q, %v; want %q", b, err, want)
		}
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("written through the link: %q", b)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(names) != 2 {
		t.Fatalf("files left: %v", names)
	}
	// A failed write leaves no temporary file.
	if err := WritePrivate(filepath.Join(p, "under-a-file"), nil); err == nil {
		t.Fatal("WritePrivate under a file succeeded")
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*")); len(names) != 2 {
		t.Fatalf("files left after a failure: %v", names)
	}
}
