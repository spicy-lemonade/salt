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
// or as a folder, nor one another extra file uses, nor an unsafe one, also
// when only the case differs. Nothing is written to the repo.
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
		"source file, case":   {x("soul.md")},
		"source folder, case": {x("MEMORIES")},
		"under a file, case":  {x("Soul.md/state.db")},
		"two extra, case":     {x("state.db"), x("State.DB")},
		"extra folder, case":  {x("dbs/state.db"), x("DBS")},
	} {
		if _, err := f.sealExtra(extra...); !errors.Is(err, ErrDuplicatePath) {
			t.Errorf("%s: %v, want ErrDuplicatePath", name, err)
		}
	}
	// A clash only because case is ignored names both paths and says why, so
	// a backup refused for it is easy to fix.
	if _, err := f.sealExtra(x("Soul.md")); err == nil || !strings.Contains(err.Error(), "SOUL.md and Soul.md would clash on macOS and Windows, since those ignore case") {
		t.Errorf("case only: %v", err)
	}
	// The folder named is the source's, whole, also when case folding
	// changes a letter's length in bytes.
	if _, err := f.sealExtra(x("Memorie\u017f")); err == nil || !strings.Contains(err.Error(), "memories and Memorie\u017f would clash on macOS and Windows") {
		t.Errorf("case only, long s: %v", err)
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

// A missing extra file stops the seal, as a source file that vanishes does.
func TestSealExtraMissingFile(t *testing.T) {
	f := newFixture(t, true)
	missing := filepath.Join(t.TempDir(), "gone.db")
	if _, err := f.sealExtra(Extra{Rel: "state.db", Path: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing extra file: %v", err)
	}
}

// Two paths clash when they are the same or one is a folder above the
// other, ignoring case, and clash only by case when they would not clash
// with case kept. Paths that only share a folder, or a beginning, do not
// clash.
func TestClash(t *testing.T) {
	for _, tc := range []struct {
		a, b            string
		clash, caseOnly bool
	}{
		{"state.db", "state.db", true, false},
		{"state.db", "State.DB", true, true},
		{"agent", "agent/state.db", true, false},
		{"Agent/state.db", "agent", true, true},
		{"a/b/c.db", "A/B", true, true},
		{"agent/state.db", "agent/SOUL.md", false, false},
		{"agent", "agent2/state.db", false, false},
		{"state.db", "state.db2", false, false},
		{"state", "state.db", false, false},
		{"a.db", "b.db", false, false},
		// Case folding can change a letter's length in bytes: the Kelvin
		// sign (3 bytes) folds to k (1 byte), and long s (2 bytes) to s.
		{"\u212a.db", "k.db", true, true},
		{"\u212a/x.db", "k", true, true},
		{"\u017ftate.db", "State.db", true, true},
		{"\u212a", "kk/x.db", false, false},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := Clash(p[0], p[1]); got != tc.clash {
				t.Errorf("Clash(%q, %q) = %v, want %v", p[0], p[1], got, tc.clash)
			}
			if got := CaseOnly(p[0], p[1]); got != tc.caseOnly {
				t.Errorf("CaseOnly(%q, %q) = %v, want %v", p[0], p[1], got, tc.caseOnly)
			}
		}
	}
}
