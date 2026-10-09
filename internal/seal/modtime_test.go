package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// setModTime gives a source file a fixed last-modified date.
func (f *fixture) setModTime(rel string, at time.Time) {
	f.t.Helper()
	if err := os.Chtimes(filepath.Join(f.src, filepath.FromSlash(rel)), time.Time{}, at); err != nil {
		f.t.Fatal(err)
	}
}

// restore restores the backup with opt into a new folder and returns it.
func (f *fixture) restore(opt RestoreOptions) string {
	f.t.Helper()
	dest := filepath.Join(f.t.TempDir(), "r")
	if _, err := Restore(f.root, f.ids(), dest, opt); err != nil {
		f.t.Fatal(err)
	}
	return dest
}

// modTime returns the last-modified date of rel under dir.
func modTime(t *testing.T, dir, rel string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// restoredModTime restores the whole backup and returns the last-modified
// date of one restored file.
func (f *fixture) restoredModTime(rel string) time.Time {
	f.t.Helper()
	return modTime(f.t, f.restore(RestoreOptions{}), rel)
}

// rewriteIndex applies fn to every entry in the repo's index.
func (f *fixture) rewriteIndex(fn func(*Entry)) []byte {
	f.t.Helper()
	ix, err := ReadIndex(f.root, f.ids(), SignatureOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range ix.Entries {
		fn(&ix.Entries[i])
	}
	return f.writeIndex(ix)
}

func TestRestoreKeepsModTimes(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			f := newFixture(t, encryptPaths)
			dates := map[string]time.Time{
				"notes/profile.md": time.Date(2024, 3, 1, 9, 30, 15, 123456789, time.UTC),
				"data/store.db":    time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
				"NOTES.md":         time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC), // before 1970
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
			if target, err := os.Readlink(filepath.Join(dest, "notes-link")); err != nil || target != "NOTES.md" {
				t.Errorf("notes-link = %q, %v", target, err)
			}
			ix, err := ReadIndex(f.root, f.ids(), SignatureOptions{})
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

// The index always holds each file's current date. A file whose date
// changed but whose content did not keeps its ciphertext; only the index is
// rewritten, and a restore gives the new date.
func TestDateOnlyChangeRecordsNewDate(t *testing.T) {
	f := newFixture(t, true)
	f.setModTime("data/store.db", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	f.seal(false)
	before := snapshot(t, f.root)

	touched := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	f.setModTime("data/store.db", touched)
	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 5 || !res.IndexNew {
		t.Fatalf("seal after a date-only change: %+v", res)
	}
	for k, v := range snapshot(t, f.root) {
		if k != repo.IndexFile && before[k] != v {
			t.Errorf("%s changed although only a date did", k)
		}
	}
	if got := f.restoredModTime("data/store.db"); !got.Equal(touched) {
		t.Fatalf("last-modified after a date-only change = %v, want %v", got, touched)
	}

	// With neither content nor dates changed, nothing is rewritten.
	if res := f.seal(false); res.Encrypted != 0 || res.IndexNew {
		t.Fatalf("seal with nothing changed: %+v", res)
	}
}

// Backups made before salt kept dates have no mtime in the index. They
// restore normally, with the time of the restore.
func TestRestoreIndexWithoutDates(t *testing.T) {
	f := newFixture(t, true)
	f.setModTime("NOTES.md", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	f.seal(false)
	b := f.rewriteIndex(func(e *Entry) { e.MTime = 0 })
	if bytes.Contains(b, []byte(`"mtime"`)) {
		t.Fatalf("index without dates still names mtime: %s", b)
	}
	start := time.Now().Add(-time.Minute)
	if got := f.restoredModTime("NOTES.md"); got.Before(start) {
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
		"earliest":    {minUnixNano, math.MinInt64},
		"latest":      {maxUnixNano, math.MaxInt64},
		"too early":   {minUnixNano.Add(-1), 0},
		"too late":    {maxUnixNano.Add(1), 0},
	} {
		if got := unixNano(tc.in); got != tc.want {
			t.Errorf("%s: unixNano = %d, want %d", name, got, tc.want)
		}
	}
}

// The index records each regular file's date to the nanosecond, and none for
// symlinks.
func TestIndexRecordsDates(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)
	ix, err := ReadIndex(f.root, f.ids(), SignatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ix.Entries {
		if e.Symlink != "" {
			if e.MTime != 0 {
				t.Errorf("symlink %s has a date", e.Path)
			}
			continue
		}
		if want := modTime(t, f.src, e.Path).UnixNano(); e.MTime != want {
			t.Errorf("%s date = %d, want %d", e.Path, e.MTime, want)
		}
	}
}

// A backup sealed before salt kept dates gains them on the next seal: only
// the index is rewritten, no file is encrypted again, and the seal after
// that changes nothing.
func TestSealAddsDatesToAnOlderBackup(t *testing.T) {
	f := newFixture(t, true)
	at := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	f.setModTime("notes/profile.md", at)
	f.seal(false)
	b := f.rewriteIndex(func(e *Entry) { e.MTime = 0 })

	// Point the cache at the dateless index, as the older salt left it.
	cPath, err := cachePath(f.cache, f.root)
	if err != nil {
		t.Fatal(err)
	}
	c := loadCache(cPath, cacheKey(f.repo.RecipientStrings, true))
	c.IndexSHA = hex.EncodeToString(sha256Of(b))
	fi, err := os.Stat(filepath.Join(f.root, repo.IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	c.IndexSize = fi.Size()
	if err := c.save(cPath); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.root)

	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 5 || !res.IndexNew {
		t.Fatalf("first seal after upgrading: %+v", res)
	}
	for k, v := range snapshot(t, f.root) {
		if k != repo.IndexFile && before[k] != v {
			t.Errorf("%s changed although only dates were added", k)
		}
	}
	if got := f.restoredModTime("notes/profile.md"); !got.Equal(at) {
		t.Fatalf("last-modified = %v, want %v", got, at)
	}
	if res := f.seal(false); res.IndexNew {
		t.Fatalf("second seal after upgrading: %+v", res)
	}
}

// One seal with every kind of change: new content, a date-only change, a
// new file and a deleted file. Only changed and new files are encrypted,
// and every restored file has the date it had when sealed.
func TestMixedChangesKeepEveryDate(t *testing.T) {
	f := newFixture(t, true)
	f.seal(false)

	f.write("notes/profile.md", "The user moved to Cork.\n")
	f.setModTime("notes/profile.md", time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC))
	f.setModTime("NOTES.md", time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC))
	f.write("notes/new.md", "new\n")
	f.setModTime("notes/new.md", time.Date(2026, 1, 3, 8, 0, 0, 0, time.UTC))
	if err := os.Remove(filepath.Join(f.src, "notes/facts.md")); err != nil {
		t.Fatal(err)
	}
	res := f.seal(false)
	if res.Encrypted != 2 || res.Reused != 3 || len(res.Removed) != 2 || !res.IndexNew {
		t.Fatalf("seal with mixed changes: %+v", res)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// Losing the cache re-encrypts everything, and the dates are still recorded.
func TestCacheLossKeepsDates(t *testing.T) {
	f := newFixture(t, true)
	f.setModTime("NOTES.md", time.Date(2022, 10, 10, 10, 10, 10, 0, time.UTC))
	f.seal(false)
	if err := os.RemoveAll(f.cache); err != nil {
		t.Fatal(err)
	}
	if res := f.seal(false); res.Encrypted != 5 {
		t.Fatalf("seal without a cache: %+v", res)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// A partial restore and a restore over an existing folder keep dates too.
func TestPartialAndForcedRestoreKeepDates(t *testing.T) {
	f := newFixture(t, true)
	at := time.Date(2021, 6, 7, 8, 9, 10, 11, time.UTC)
	f.setModTime("notes/profile.md", at)
	f.seal(false)

	dest := f.restore(RestoreOptions{Paths: []string{"notes"}})
	if got := modTime(t, dest, "notes/profile.md"); !got.Equal(at) {
		t.Errorf("partial restore: last-modified = %v, want %v", got, at)
	}
	if _, err := os.Stat(filepath.Join(dest, "NOTES.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial restore wrote NOTES.md: %v", err)
	}

	res, err := Restore(f.root, f.ids(), dest, RestoreOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := modTime(t, dest, "notes/profile.md"); !got.Equal(at) {
		t.Errorf("forced restore: last-modified = %v, want %v", got, at)
	}
	if got := modTime(t, res.MovedAside, "notes/profile.md"); !got.Equal(at) {
		t.Errorf("moved-aside copy: last-modified = %v, want %v", got, at)
	}
}

func sha256Of(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
