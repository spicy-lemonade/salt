//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitHubFileLimit is the size above which GitHub refuses a pushed file.
const gitHubFileLimit = 100 << 20

// writeNoise writes size bytes that do not compress to path, a megabyte at a
// time, and returns their SHA-256.
func writeNoise(t *testing.T, path string, size int) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(f, h), rand.NewChaCha8([32]byte{6}), int64(size)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// A file over GitHub's limit is sealed in parts that are each under it,
// passes the real hook, is pushed, and restores identically from a fresh
// clone. A file under the limit stays one object.
func TestSealSplitsAFileOverTheLimit(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	big := filepath.Join(b.src, "memory.db")
	os.MkdirAll(b.src, 0o755)
	want := writeNoise(t, big, 105<<20)
	edited := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(big, time.Time{}, edited); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(b.src, "MEMORY.md"), "small\n")
	// Ciphertext never compresses and has no deltas, so git's attempts at
	// both only take time: over half this test's, and more on a slow macOS
	// runner. They are turned off in this clone alone, so what salt writes
	// and checks is unchanged.
	e.must(b.dir, "git", "config", "core.compression", "0")
	write(t, filepath.Join(b.dir, ".git", "info", "attributes"), "*.age -delta\n")

	e.must(b.base, "salt", "seal", "--prune", b.src, b.dir)
	e.must(b.dir, "git", "add", "-A")
	b.commitOn(t, "2026-09-01", "backup") // the hook checks every part
	e.must(b.dir, "git", "push", "-q", "origin", "main")

	// Every pushed file is under the limit; the big file is in 3 parts of
	// about 45 MiB and the small one is a single object.
	var objects int
	for _, line := range strings.Split(strings.TrimSpace(e.must(b.remote, "git", "ls-tree", "-r", "-l", "main")), "\n") {
		fields := strings.Fields(line)
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			t.Fatalf("ls-tree line %q: %v", line, err)
		}
		if size >= gitHubFileLimit {
			t.Errorf("pushed %s is %d bytes, over GitHub's limit", fields[4], size)
		}
		if strings.HasPrefix(fields[4], "objects/") {
			objects++
		}
	}
	if objects != 4 {
		t.Errorf("pushed %d objects, want 3 parts and 1 small file", objects)
	}

	if out := e.must(b.base, "salt", "verify", b.dir); !strings.Contains(out, "All 2 files") {
		t.Fatalf("verify:\n%s", out)
	}
	if out := e.must(b.base, "salt", "doctor", b.dir); strings.Contains(out, "GitHub") {
		t.Fatalf("doctor warns about the parts:\n%s", out)
	}
	e.must(b.base, "salt", "seal", "--prune", b.src, b.dir)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an unchanged split file changed the repo:\n%s", st)
	}

	clone := filepath.Join(b.base, "clone")
	e.must(b.base, "git", "clone", "-q", b.remote, clone)
	dest := filepath.Join(b.base, "restored")
	e.must(b.base, "salt", "restore", clone, "--to", dest)
	restored := filepath.Join(dest, "memory.db")
	if got := fileSHA(t, restored); got != want {
		t.Fatal("memory.db differs after restore")
	}
	if fi, err := os.Stat(restored); err != nil || !fi.ModTime().Equal(edited) {
		t.Fatalf("restored memory.db: %v, want last-modified %v", err, edited)
	}
}
