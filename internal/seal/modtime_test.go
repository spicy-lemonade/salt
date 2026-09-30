package seal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setModTime gives a source file a fixed last-modified date.
func (f *fixture) setModTime(rel string, at time.Time) {
	f.t.Helper()
	if err := os.Chtimes(filepath.Join(f.src, filepath.FromSlash(rel)), time.Time{}, at); err != nil {
		f.t.Fatal(err)
	}
}

// restoredModTime restores the whole backup and returns the last-modified
// date of one restored file.
func (f *fixture) restoredModTime(rel string) time.Time {
	f.t.Helper()
	dest := filepath.Join(f.t.TempDir(), "r")
	if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err != nil {
		f.t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return fi.ModTime()
}

// rewriteIndex applies fn to every entry in the repo's index.
func (f *fixture) rewriteIndex(fn func(*Entry)) []byte {
	f.t.Helper()
	ix, err := ReadIndex(f.root, f.ids())
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range ix.Entries {
		fn(&ix.Entries[i])
	}
	b, _, err := ix.marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := writeIndex(f.root, b, f.repo.Recipients); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func TestRestoreKeepsModTimes(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			f := newFixture(t, encryptPaths)
			dates := map[string]time.Time{
				"memories/USER.md":            time.Date(2024, 3, 1, 9, 30, 15, 123456789, time.UTC),
				"mnemosyne/data/mnemosyne.db": time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
				"SOUL.md":                     time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC), // before 1970
			}
			for rel, at := range dates {
				f.setModTime(rel, at)
			}
			f.seal(false)
			dest := filepath.Join(t.TempDir(), "r")
			if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err != nil {
				t.Fatal(err)
			}
			for rel, at := range dates {
				fi, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				if !fi.ModTime().Equal(at) {
					t.Errorf("%s last-modified = %v, want %v", rel, fi.ModTime(), at)
				}
			}
			// The link keeps its target; symlink dates are not recorded.
			if target, err := os.Readlink(filepath.Join(dest, "soul-link")); err != nil || target != "SOUL.md" {
				t.Errorf("soul-link = %q, %v", target, err)
			}
			ix, err := ReadIndex(f.root, f.ids())
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ix.Entries {
				if e.Symlink != "" && e.MTime != 0 {
					t.Errorf("symlink %s has a date %d", e.Path, e.MTime)
				}
			}
		})
	}
}

// A copy that only gets a new date, such as sqlite3 .backup of an unchanged
// database, must not change the repo, so the date recorded with the content
// is kept. Changed content takes the new date.
func TestUnchangedContentKeepsItsDate(t *testing.T) {
	f := newFixture(t, true)
	first := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	f.setModTime("mnemosyne/data/mnemosyne.db", first)
	f.seal(false)
	before := snapshot(t, f.root)

	f.setModTime("mnemosyne/data/mnemosyne.db", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	res := f.seal(false)
	if res.Encrypted != 0 || res.IndexNew {
		t.Fatalf("seal after a date-only change: %+v", res)
	}
	for k, v := range snapshot(t, f.root) {
		if before[k] != v {
			t.Errorf("%s changed although only a date did", k)
		}
	}
	if got := f.restoredModTime("mnemosyne/data/mnemosyne.db"); !got.Equal(first) {
		t.Fatalf("last-modified after a date-only change = %v, want %v", got, first)
	}

	changed := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	f.write("mnemosyne/data/mnemosyne.db", "SQLite format 3\x00new")
	f.setModTime("mnemosyne/data/mnemosyne.db", changed)
	if res := f.seal(false); res.Encrypted != 1 || !res.IndexNew {
		t.Fatalf("seal after a content change: %+v", res)
	}
	if got := f.restoredModTime("mnemosyne/data/mnemosyne.db"); !got.Equal(changed) {
		t.Fatalf("last-modified after a content change = %v, want %v", got, changed)
	}
}

// A cache written before salt kept dates has none, so seal takes each
// unchanged file's current date instead of recording nothing.
func TestCacheWithoutDatesUsesFileDates(t *testing.T) {
	f := newFixture(t, true)
	at := time.Date(2023, 5, 6, 7, 8, 9, 0, time.UTC)
	f.setModTime("SOUL.md", at)
	f.seal(false)
	cPath, err := cachePath(f.cache, f.root)
	if err != nil {
		t.Fatal(err)
	}
	c := loadCache(cPath, cacheKey(f.repo.RecipientStrings, true))
	for k, ce := range c.Files {
		ce.MTime = 0
		c.Files[k] = ce
	}
	if err := c.save(cPath); err != nil {
		t.Fatal(err)
	}
	if res := f.seal(false); res.Reused != 5 {
		t.Fatalf("seal with an old cache: %+v", res)
	}
	if got := f.restoredModTime("SOUL.md"); !got.Equal(at) {
		t.Fatalf("last-modified = %v, want %v", got, at)
	}
	if c := loadCache(cPath, cacheKey(f.repo.RecipientStrings, true)); c.Files["SOUL.md"].MTime != at.UnixNano() {
		t.Fatalf("cache date = %d, want %d", c.Files["SOUL.md"].MTime, at.UnixNano())
	}
}

// Backups made before salt kept dates have no mtime in the index. They
// restore normally, with the time of the restore.
func TestRestoreIndexWithoutDates(t *testing.T) {
	f := newFixture(t, true)
	f.setModTime("SOUL.md", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	f.seal(false)
	b := f.rewriteIndex(func(e *Entry) { e.MTime = 0 })
	if bytes.Contains(b, []byte(`"mtime"`)) {
		t.Fatalf("index without dates still names mtime: %s", b)
	}
	start := time.Now().Add(-time.Minute)
	if got := f.restoredModTime("SOUL.md"); got.Before(start) {
		t.Fatalf("last-modified = %v, want the time of the restore", got)
	}
}

// A date that cannot be set fails the restore and leaves nothing behind.
func TestRestoreModTimeError(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	t.Cleanup(func() { chtimes = os.Chtimes })
	chtimes = func(string, time.Time, time.Time) error { return errors.New("read-only file system") }
	parent := t.TempDir()
	dest := filepath.Join(parent, "r")
	_, err := Restore(f.root, f.ids(), dest, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "setting its last-modified date: read-only file system") {
		t.Fatalf("restore with a failing chtimes: %v", err)
	}
	if left, _ := os.ReadDir(parent); len(left) != 0 {
		t.Fatalf("restore left %v behind", left)
	}
}

func TestUnixNano(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 1, time.UTC)
	for name, tc := range map[string]struct {
		in   time.Time
		want int64
	}{
		"normal":      {at, at.UnixNano()},
		"before 1970": {time.Unix(-86400, 0), -86400 * int64(time.Second)},
		"year 1":      {time.Time{}, 0},
		"year 1600":   {time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), 0},
		"year 3000":   {time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), 0},
	} {
		if got := unixNano(tc.in); got != tc.want {
			t.Errorf("%s: unixNano = %d, want %d", name, got, tc.want)
		}
	}
}
