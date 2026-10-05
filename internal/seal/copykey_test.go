package seal

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func assertCopyKey(t *testing.T, key string) {
	t.Helper()
	if _, err := hex.DecodeString(key); err != nil || len(key) != copyKeyLen {
		t.Fatalf("copy key %q is not %d hex characters", key, copyKeyLen)
	}
}

// The key is made once per repo, kept owner-only, and given back after.
func TestCopyKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	a, b := t.TempDir(), t.TempDir()
	ka, err := CopyKey(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	assertCopyKey(t, ka)
	if again, err := CopyKey(dir, a); err != nil || again != ka {
		t.Fatalf("second CopyKey = %q, %v; want %q", again, err, ka)
	}
	if kb, err := CopyKey(dir, b); err != nil || kb == ka {
		t.Fatalf("another repo's key = %q, %v", kb, err)
	}
	p, _ := RepoFile(dir, a, "copykey-", "")
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", fi, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v, %v", fi, err)
	}
	// A damaged key is replaced.
	for _, damaged := range []string{"not a key", "0123456789abcdef0123456789abcdeg", ka + "0"} {
		os.WriteFile(p, []byte(damaged), 0o600)
		k, err := CopyKey(dir, a)
		if err != nil || k == damaged {
			t.Fatalf("after %q: CopyKey = %q, %v", damaged, k, err)
		}
		assertCopyKey(t, k)
	}
}

func TestCopyKeyUnwritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := CopyKey(file, t.TempDir()); err == nil {
		t.Fatal("CopyKey under a file succeeded")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to any folder")
	}
	readOnly := t.TempDir()
	os.Chmod(readOnly, 0o500)
	if _, err := CopyKey(readOnly, t.TempDir()); err == nil {
		t.Fatal("CopyKey in a read-only folder succeeded")
	}
}

// A relative repo path needs the current folder, which may be gone.
func TestCopyKeyWithoutCurrentFolder(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	os.Mkdir(gone, 0o700)
	t.Chdir(gone)
	os.Remove(gone)
	if _, err := os.Getwd(); err == nil {
		t.Skip("this system still reports a removed current folder")
	}
	if _, err := CopyKey(t.TempDir(), "repo"); err == nil {
		t.Fatal("CopyKey without a current folder succeeded")
	}
}
