package seal

import (
	"os"
	"path/filepath"
	"testing"
)

// The key is made once per repo, kept owner-only, and given back after.
func TestCopyKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	a, b := t.TempDir(), t.TempDir()
	ka, err := CopyKey(dir, a)
	if err != nil || !isCopyKey(ka) {
		t.Fatalf("CopyKey = %q, %v", ka, err)
	}
	if again, err := CopyKey(dir, a); err != nil || again != ka {
		t.Fatalf("second CopyKey = %q, %v; want %q", again, err, ka)
	}
	if kb, err := CopyKey(dir, b); err != nil || kb == ka {
		t.Fatalf("another repo's key = %q, %v", kb, err)
	}
	p, _ := repoFile(dir, a, "copykey-", "")
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", fi, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v, %v", fi, err)
	}
	// A damaged key is replaced.
	os.WriteFile(p, []byte("not a key"), 0o600)
	if k, err := CopyKey(dir, a); err != nil || k == ka || !isCopyKey(k) {
		t.Fatalf("after damage CopyKey = %q, %v", k, err)
	}
}

func TestCopyKeyUnwritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := CopyKey(file, t.TempDir()); err == nil {
		t.Fatal("CopyKey under a file succeeded")
	}
}

func TestIsCopyKey(t *testing.T) {
	for s, want := range map[string]bool{
		"0123456789abcdef0123456789abcdef":  true,
		"0123456789abcdef0123456789abcde":   false,
		"0123456789abcdef0123456789abcdeg":  false,
		"0123456789abcdef0123456789abcdef0": false,
	} {
		if isCopyKey(s) != want {
			t.Errorf("%q: %v", s, !want)
		}
	}
}
