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
	return Seal(f.src, f.repo, Options{CacheDir: f.cache, Extra: extra})
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
		ix, err := ReadIndex(f.root, f.ids())
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

// A missing extra file stops the seal, as a source file that vanishes does.
func TestSealExtraMissingFile(t *testing.T) {
	f := newFixture(t, true)
	missing := filepath.Join(t.TempDir(), "gone.db")
	if _, err := f.sealExtra(Extra{Rel: "state.db", Path: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing extra file: %v", err)
	}
}
