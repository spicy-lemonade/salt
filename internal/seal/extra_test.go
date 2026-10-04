package seal

import (
	"errors"
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

// Names are compared exactly, case included: an extra file whose path
// differs from a source file or folder, or another extra file, only by case
// is sealed and restores beside it.
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
	dest := f.restore(RestoreOptions{})
	for rel, content := range want {
		if b, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(b) != content {
			t.Errorf("restored %s = %q, %v; want %q", rel, b, err, content)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SOUL.md")); err != nil || string(b) != "be kind\n" {
		t.Errorf("restored SOUL.md = %q, %v", b, err)
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
// other, compared exactly, case included. Paths that differ only by case,
// only share a folder, or only share a beginning do not clash.
func TestClash(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		clash bool
	}{
		{"state.db", "state.db", true},
		{"agent", "agent/state.db", true},
		{"a/b/c.db", "a/b", true},
		{"state.db", "State.DB", false},
		{"Agent/state.db", "agent", false},
		{"a/b/c.db", "A/B", false},
		// The Kelvin sign and long s are not k and s.
		{"\u212a.db", "k.db", false},
		{"\u017ftate.db", "state.db", false},
		{"agent/state.db", "agent/SOUL.md", false},
		{"agent", "agent2/state.db", false},
		{"state.db", "state.db2", false},
		{"state", "state.db", false},
		{"a.db", "b.db", false},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := Clash(p[0], p[1]); got != tc.clash {
				t.Errorf("Clash(%q, %q) = %v, want %v", p[0], p[1], got, tc.clash)
			}
		}
	}
}
