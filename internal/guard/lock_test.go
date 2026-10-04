package guard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A second lock on the same file is refused at once until the first is
// released. Each open file holds its own lock, so this works in one process.
func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache", "lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	again, err := Lock(p)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v, %v", fi, err)
	}
	// A folder where the lock file goes cannot be opened.
	if _, err := Lock(t.TempDir()); err == nil {
		t.Fatal("locked a folder")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocked, nil, 0o600)
	if _, err := Lock(filepath.Join(blocked, "lock")); err == nil {
		t.Fatal("made a folder over a file")
	}
}
