package seal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
		ix, err := ReadIndex(f.root, f.ids(), nil, false)
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
		"source file":         {x("NOTES.md")},
		"source folder":       {x("notes")},
		"under a source file": {x("NOTES.md/state.db")},
		"two extra files":     {x("state.db"), x("state.db")},
		"extra folder":        {x("dbs/state.db"), x("dbs")},
		"cleaned path":        {x("./notes/profile.md")},
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
	want := map[string]string{"notes.md": "lower", "NOTES/state.db": "upper", "state.db": "a", "State.DB": "b"}
	var extra []Extra
	for rel, content := range want {
		extra = append(extra, x(rel, content))
	}
	if _, err := f.sealExtra(extra...); err != nil {
		t.Fatal(err)
	}
	ix, err := ReadIndex(f.root, f.ids(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sealed := map[string]bool{}
	for _, e := range ix.Entries {
		sealed[e.Path] = true
	}
	for _, rel := range []string{"notes.md", "NOTES.md", "NOTES/state.db", "notes/profile.md", "state.db", "State.DB"} {
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
	if b, err := os.ReadFile(filepath.Join(dest, "NOTES.md")); err != nil || string(b) != "be kind\n" {
		t.Errorf("restored NOTES.md = %q, %v", b, err)
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
		"NOTES.md and notes.md differ only by case":          {x("notes.md")},
		"notes and NOTES differ only by case":                {x("NOTES")},
		"NOTES.md and Notes.md/state.db differ only by case": {x("Notes.md/state.db")},
		"state.db and State.DB differ only by case":          {x("state.db"), x("State.DB")},
		"notes and Note\u017f differ only by case":           {x("Note\u017f")},
	} {
		_, err := f.sealExtra(extra...)
		if !errors.Is(err, ErrDuplicatePath) || !strings.Contains(err.Error(), want+", and with --plain-paths the repo would keep them as one file on macOS and Windows") {
			t.Errorf("%v: %v", extra, err)
		}
	}
	// An exact clash keeps the plain message.
	if _, err := f.sealExtra(x("NOTES.md")); err == nil || err.Error() != ErrDuplicatePath.Error()+": NOTES.md" {
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

// A live extra file's permissions and date are read when it is sealed, not
// taken from when it was found, since the tool may have changed it since.
func TestSealLiveExtraReadsItsOwnDetails(t *testing.T) {
	f := newFixture(t, true)
	p := extraCopy(t, "changed since it was found")
	os.Chmod(p, 0o640)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	os.Chtimes(p, at, at)
	stale := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.sealExtra(Extra{Rel: "a/notes.md", Path: p, Mode: 0o600, ModTime: stale, Live: true}); err != nil {
		t.Fatal(err)
	}
	if got := modTime(t, f.restore(RestoreOptions{}), "a/notes.md"); !got.Equal(at) {
		t.Fatalf("restored notes.md is dated %v, want %v", got, at)
	}
	ix, err := ReadIndex(f.root, f.ids(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ix.Entries {
		if e.Path == "a/notes.md" && e.Mode != 0o640 {
			t.Fatalf("index records mode %o, want 640", e.Mode)
		}
	}
}

// A live extra file that is no longer a file, such as one a tool replaced
// with a named pipe, is left out as a deleted one is, and never opened:
// opening a named pipe would wait for ever.
func TestSealLiveExtraNotAFile(t *testing.T) {
	f := newFixture(t, true)
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	res, err := f.sealExtra(Extra{Rel: "a/pipe", Path: pipe, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Gone, []string{"a/pipe"}) {
		t.Fatalf("gone = %v", res.Gone)
	}
}

// A path missing in the repo while a live file is sealed is an error, never
// taken for the file having been deleted, which would leave it out.
func TestSealLiveExtraRepoPathMissing(t *testing.T) {
	f := newFixture(t, true)
	p := extraCopy(t, "kept")
	hashedHook = func(string) { os.RemoveAll(f.root) }
	t.Cleanup(func() { hashedHook = nil })
	_, err := Seal("", f.repo, Options{CacheDir: f.cache, Signer: f.signer, Extra: []Extra{{Rel: "a/kept.md", Path: p, Live: true}}})
	if err == nil || !strings.HasPrefix(err.Error(), "a/kept.md: ") {
		t.Fatalf("seal: %v", err)
	}
}

// A live extra file deleted before it is read, as a tool's files can be, is
// left out of the backup and listed as gone, and the seal goes on. So is one
// deleted after it was measured but before it was encrypted, and one sealed
// last time, whose ciphertext and cache entry are then dropped.
func TestSealLiveExtraGone(t *testing.T) {
	f := newFixture(t, true)
	kept := extraCopy(t, "kept")
	late := filepath.Join(t.TempDir(), "late.md")
	os.WriteFile(late, []byte("deleted after it was measured"), 0o600)
	hashedHook = func(p string) {
		if p == late {
			os.Remove(p)
		}
	}
	t.Cleanup(func() { hashedHook = nil })
	res, err := f.sealExtra(
		Extra{Rel: "a/kept.md", Path: kept, Live: true},
		Extra{Rel: "a/gone.md", Path: filepath.Join(t.TempDir(), "gone.md"), Live: true},
		Extra{Rel: "a/late.md", Path: late, Live: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Gone, []string{"a/gone.md", "a/late.md"}) || res.Files != 6 {
		t.Fatalf("seal: %+v", res)
	}
	dest := f.restore(RestoreOptions{})
	if b, err := os.ReadFile(filepath.Join(dest, "a", "kept.md")); err != nil || string(b) != "kept" {
		t.Fatalf("restored kept.md = %q, %v", b, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dest, "a")); len(entries) != 1 {
		t.Fatalf("restored a/ holds %v", entries)
	}

	os.Remove(kept)
	res, err = f.sealExtra(Extra{Rel: "a/kept.md", Path: kept, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Gone, []string{"a/kept.md"}) || res.Files != 5 || len(res.Removed) != 1 {
		t.Fatalf("seal: %+v", res)
	}
	if c, _ := f.loadCache(); len(c.Files) != 5 {
		t.Fatalf("the cache still holds %d files", len(c.Files))
	}
	assertAllCiphertext(t, f.root, true)
	ix, err := ReadIndex(f.root, f.ids(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ix.Entries {
		if strings.HasPrefix(e.Path, "a/") {
			t.Fatalf("the index still lists %s", e.Path)
		}
	}
}

// Two paths clash when they are the same or one is a folder above the
// other. Clash, the rule with encrypted paths, compares them exactly, case
// included. With plain paths, case is ignored, part by part, also for letters
// whose length in bytes changes when folded. Paths that only share a folder,
// or a beginning, never clash.
func TestClash(t *testing.T) {
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
		{"agent/state.db", "agent/NOTES.md", false, false},
		{"agent", "agent2/state.db", false, false},
		{"state.db", "state.db2", false, false},
		{"state", "state.db", false, false},
		{"a.db", "b.db", false, false},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := Clash(p[0], p[1]); got != tc.exact {
				t.Errorf("Clash(%q, %q) = %v, want %v", p[0], p[1], got, tc.exact)
			}
			// Paths are looked up rather than compared, and the lookup must
			// agree with Clash, and ignore case only when folding.
			for fold, want := range map[bool]bool{false: tc.exact, true: tc.folds} {
				tk := newTaken(fold)
				tk.add(p[0])
				if _, _, got := tk.clash(p[1]); got != want {
					t.Errorf("taken(fold %v) of %q clashes with %q = %v, want %v", fold, p[0], p[1], got, want)
				}
			}
		}
	}
}

// Many extra files are added in time that grows with their number, not its
// square. Comparing every pair of 50,000 paths took over a minute.
func TestAddExtraScales(t *testing.T) {
	extra := make([]Extra, 50_000)
	for i := range extra {
		extra[i] = Extra{Rel: fmt.Sprintf("blobs/%02x/%04x/%08x", i%256, i%65536, i)}
	}
	start := time.Now()
	for _, fold := range []bool{false, true} {
		items, err := addExtra(nil, extra, fold)
		if err != nil || len(items) != len(extra) {
			t.Fatalf("addExtra: %d items, %v", len(items), err)
		}
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("adding 50,000 extra files twice took %v", d)
	}
}

// With no source folder, only the extra files are sealed, and they restore
// in their own folders.
func TestSealOnlyExtra(t *testing.T) {
	f := newFixture(t, true)
	at := time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC)
	res, err := Seal("", f.repo, Options{CacheDir: f.cache, Signer: f.signer, Extra: []Extra{
		{Rel: "tool/notes.md", Path: extraCopy(t, "notes"), Mode: 0o644, ModTime: at},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Encrypted != 1 {
		t.Fatalf("seal: %+v", res)
	}
	dest := f.restore(RestoreOptions{})
	b, err := os.ReadFile(filepath.Join(dest, "tool", "notes.md"))
	if err != nil || string(b) != "notes" {
		t.Fatalf("restored notes.md = %q, %v", b, err)
	}
	// The file limit names the backup when there is no source folder.
	old := MaxIndexEntries
	MaxIndexEntries = 0
	t.Cleanup(func() { MaxIndexEntries = old })
	if _, err := Seal("", f.repo, Options{CacheDir: f.cache, Signer: f.signer, Extra: []Extra{{Rel: "a", Path: extraCopy(t, "a")}}}); err == nil || !strings.HasPrefix(err.Error(), "the backup has 1 files") {
		t.Fatalf("seal over the limit: %v", err)
	}
}

// FirstClash finds the first path that clashes with one before it, and
// which one, by the rule for the repo's paths.
func TestFirstClash(t *testing.T) {
	rels := []string{"a/b.md", "c.md", "A/B.md", "a"}
	if i, j, ok := FirstClash(rels, false); !ok || i != 3 || j != 0 {
		t.Errorf("exact: %d %d %v", i, j, ok)
	}
	if i, j, ok := FirstClash(rels, true); !ok || i != 2 || j != 0 {
		t.Errorf("folded: %d %d %v", i, j, ok)
	}
	if _, _, ok := FirstClash([]string{"a/b", "a/c", "b"}, true); ok {
		t.Error("paths sharing a folder clashed")
	}
	// A path clashing with the folder above a file names that folder.
	tk := newTaken(true)
	tk.add("Agent/x/state.db")
	if j, other, ok := tk.clash("agent/X"); !ok || j != 0 || other != "Agent/x" {
		t.Errorf("folder: %d %q %v", j, other, ok)
	}
}
