package private

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
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
		if err := Write(p, []byte(want)); err != nil {
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
	if err := Write(filepath.Join(p, "under-a-file"), nil); err == nil {
		t.Fatal("Write under a file succeeded")
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*")); len(names) != 2 {
		t.Fatalf("files left after a failure: %v", names)
	}
}
