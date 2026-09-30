package seal

import (
	"bytes"
	"errors"
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

// The index always holds each file's current date. A file whose date
// changed but whose content did not keeps its ciphertext; only the index is
// rewritten, and a restore gives the new date.
func TestDateOnlyChangeRecordsNewDate(t *testing.T) {
	f := newFixture(t, true)
	f.setModTime("mnemosyne/data/mnemosyne.db", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	f.seal(false)
	before := snapshot(t, f.root)

	touched := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	f.setModTime("mnemosyne/data/mnemosyne.db", touched)
	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 5 || !res.IndexNew {
		t.Fatalf("seal after a date-only change: %+v", res)
	}
	for k, v := range snapshot(t, f.root) {
		if k != repo.IndexFile && before[k] != v {
			t.Errorf("%s changed although only a date did", k)
		}
	}
	if got := f.restoredModTime("mnemosyne/data/mnemosyne.db"); !got.Equal(touched) {
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
