package seal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// extraCopy writes content to a file outside the source tree, standing in
// for a safe copy of a live database, dated "now" as a fresh copy is.
func extraCopy(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "0.db")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) sealExtra(extra ...Extra) (*Result, error) {
	f.t.Helper()
	return Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer, Extra: extra})
}

// An extra file is sealed at its own path with the given permissions and
// date, not the copy's, and restores like any other file.
func TestSealExtraRoundTrip(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		f := newFixture(t, encryptPaths)
		at := time.Date(2025, 4, 5, 6, 7, 8, 9, time.UTC)
		copy := extraCopy(t, "SQLite format 3\x00live pages")
		res, err := f.sealExtra(Extra{Rel: "state.db", Path: copy, Mode: 0o640, ModTime: at})
		if err != nil {
			t.Fatal(err)
		}
		if res.Files != 6 || res.Encrypted != 6 {
			t.Fatalf("seal: %+v", res)
		}
		assertAllCiphertext(t, f.root, encryptPaths)
		dest := f.restore(RestoreOptions{})
		b, err := os.ReadFile(filepath.Join(dest, "state.db"))
		if err != nil || string(b) != "SQLite format 3\x00live pages" {
			t.Fatalf("restored state.db = %q, %v", b, err)
		}
		if got := modTime(t, dest, "state.db"); !got.Equal(at) {
			t.Fatalf("restored state.db is dated %v, want %v", got, at)
		}
		// Restore makes files owner-only, so the mode is checked in the index.
		ix, err := ReadIndex(f.root, f.ids(), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ix.Entries {
			if e.Path == "state.db" && e.Mode != 0o640 {
				t.Fatalf("index records mode %o, want 640", e.Mode)
			}
		}
		// The rest of the tree is untouched by the extra file.
		os.Remove(filepath.Join(dest, "state.db"))
		assertTreesEqual(t, f.src, dest)
	}
}

// A fresh copy of an unchanged database is a new file with the same bytes.
// It keeps its ciphertext, so the repo does not change.
func TestSealExtraUnchangedChangesNothing(t *testing.T) {
	f := newFixture(t, true)
	at := time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC)
	extra := func() Extra {
		return Extra{Rel: "dbs/state.db", Path: extraCopy(t, "SQLite format 3\x00same"), Mode: 0o644, ModTime: at}
	}
	if _, err := f.sealExtra(extra()); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.root)
	res, err := f.sealExtra(extra())
	if err != nil {
		t.Fatal(err)
	}
	if res.Encrypted != 0 || res.Reused != 6 || res.IndexNew || len(res.Removed) != 0 {
		t.Fatalf("second seal: %+v", res)
	}
	for k, v := range before {
		if snapshot(t, f.root)[k] != v {
			t.Errorf("%s changed although nothing did", k)
		}
	}
}

// An extra file may not take a path the source tree already uses, as a file
// or as a folder, nor one another extra file uses, nor an unsafe one. Nothing
// is written to the repo.
func TestSealExtraRefusesClashes(t *testing.T) {
	f := newFixture(t, true)
	copy := extraCopy(t, "SQLite format 3\x00")
	x := func(rel string) Extra { return Extra{Rel: rel, Path: copy, Mode: 0o644} }
	for name, extra := range map[string][]Extra{
		"source file":         {x("SOUL.md")},
		"source folder":       {x("memories")},
		"under a source file": {x("SOUL.md/state.db")},
		"two extra files":     {x("state.db"), x("state.db")},
		"extra folder":        {x("dbs/state.db"), x("dbs")},
		"cleaned path":        {x("./memories/USER.md")},
	} {
		if _, err := f.sealExtra(extra...); !errors.Is(err, ErrDuplicatePath) {
			t.Errorf("%s: %v, want ErrDuplicatePath", name, err)
		}
	}
	for _, rel := range []string{"", "/abs.db", "../up.db", "."} {
		if _, err := f.sealExtra(x(rel)); err == nil || !strings.Contains(err.Error(), "unsafe path") {
			t.Errorf("%q: %v, want unsafe path", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "index.age")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused seal wrote the index: %v", err)
	}
}

// caseSensitive reports whether the file system holding dir tells names
// apart by case. macOS and Windows usually do not.
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "case"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(dir, "case"))
	_, err := os.Stat(filepath.Join(dir, "CASE"))
	return errors.Is(err, fs.ErrNotExist)
}

// With encrypted paths, names are compared exactly, case included: an extra
// file whose path differs from a source file or folder, or another extra
// file, only by case is sealed beside it. It restores beside it where the
// file system tells the names apart, and elsewhere restore refuses rather
// than write one over the other.
func TestSealExtraKeepsCase(t *testing.T) {
	f := newFixture(t, true)
	x := func(rel, content string) Extra {
		return Extra{Rel: rel, Path: extraCopy(t, content), Mode: 0o644}
	}
	want := map[string]string{"soul.md": "lower", "MEMORIES/state.db": "upper", "state.db": "a", "State.DB": "b"}
	var extra []Extra
	for rel, content := range want {
		extra = append(extra, x(rel, content))
	}
	if _, err := f.sealExtra(extra...); err != nil {
		t.Fatal(err)
	}
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		t.Fatal(err)
	}
	sealed := map[string]bool{}
	for _, e := range ix.Entries {
		sealed[e.Path] = true
	}
	for _, rel := range []string{"soul.md", "SOUL.md", "MEMORIES/state.db", "memories/USER.md", "state.db", "State.DB"} {
		if !sealed[rel] {
			t.Errorf("%s is not in the index", rel)
		}
	}
	dest := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(f.root, f.ids(), dest, RestoreOptions{})
	if !caseSensitive(t, filepath.Dir(dest)) {
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("restore onto a file system that ignores case: %v, want it refused", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range want {
		if b, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(b) != content {
			t.Errorf("restored %s = %q, %v; want %q", rel, b, err, content)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SOUL.md")); err != nil || string(b) != "be kind\n" {
		t.Errorf("restored SOUL.md = %q, %v", b, err)
	}
}

// With plain paths, each path is also a file name in the repo, which macOS
// and Windows would not tell apart by case, so an extra file whose path
// differs from another only by case is refused, saying why. Nothing is
// written to the repo.
func TestSealExtraPlainPathsRefusesCase(t *testing.T) {
	f := newFixture(t, false)
	copy := extraCopy(t, "SQLite format 3\x00")
	x := func(rel string) Extra { return Extra{Rel: rel, Path: copy, Mode: 0o644} }
	for want, extra := range map[string][]Extra{
		"SOUL.md and soul.md differ only by case":          {x("soul.md")},
		"memories and MEMORIES differ only by case":        {x("MEMORIES")},
		"SOUL.md and Soul.md/state.db differ only by case": {x("Soul.md/state.db")},
		"state.db and State.DB differ only by case":        {x("state.db"), x("State.DB")},
		"memories and Memorie\u017f differ only by case":   {x("Memorie\u017f")},
	} {
		_, err := f.sealExtra(extra...)
		if !errors.Is(err, ErrDuplicatePath) || !strings.Contains(err.Error(), want+", and with --plain-paths the repo would keep them as one file on macOS and Windows") {
			t.Errorf("%v: %v", extra, err)
		}
	}
	// An exact clash keeps the plain message.
	if _, err := f.sealExtra(x("SOUL.md")); err == nil || err.Error() != ErrDuplicatePath.Error()+": SOUL.md" {
		t.Errorf("exact clash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "index.age")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused seal wrote the index: %v", err)
	}
}

// A missing extra file stops the seal, as a source file that vanishes does.
func TestSealExtraMissingFile(t *testing.T) {
	f := newFixture(t, true)
	missing := filepath.Join(t.TempDir(), "gone.db")
	if _, err := f.sealExtra(Extra{Rel: "state.db", Path: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing extra file: %v", err)
	}
}

// Two paths clash when they are the same or one is a folder above the
// other. Clash, the rule with encrypted paths, compares them exactly, case
// included. With plain paths, case is ignored, part by part, also for letters
// whose length in bytes changes when folded. Paths that only share a folder,
// or a beginning, never clash.
func TestClash(t *testing.T) {
	plain := ClashRule(false)
	for _, tc := range []struct {
		a, b         string
		exact, folds bool
	}{
		{"state.db", "state.db", true, true},
		{"agent", "agent/state.db", true, true},
		{"a/b/c.db", "a/b", true, true},
		{"state.db", "State.DB", false, true},
		{"Agent/state.db", "agent", false, true},
		{"a/b/c.db", "A/B", false, true},
		// The Kelvin sign (3 bytes) folds to k (1 byte), and long s to s.
		{"\u212a.db", "k.db", false, true},
		{"\u212a/x.db", "k", false, true},
		{"\u017ftate.db", "State.db", false, true},
		{"\u212a", "kk/x.db", false, false},
		{"agent/state.db", "agent/SOUL.md", false, false},
		{"agent", "agent2/state.db", false, false},
		{"state.db", "state.db2", false, false},
		{"state", "state.db", false, false},
		{"a.db", "b.db", false, false},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := Clash(p[0], p[1]); got != tc.exact {
				t.Errorf("Clash(%q, %q) = %v, want %v", p[0], p[1], got, tc.exact)
			}
			if got := ClashRule(true)(p[0], p[1]); got != tc.exact {
				t.Errorf("ClashRule(true)(%q, %q) = %v, want %v", p[0], p[1], got, tc.exact)
			}
			if got := plain(p[0], p[1]); got != tc.folds {
				t.Errorf("ClashRule(false)(%q, %q) = %v, want %v", p[0], p[1], got, tc.folds)
			}
		}
	}
}
