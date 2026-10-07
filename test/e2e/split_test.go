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

// A file over GitHub's limit is sealed in chunks that are each far under it,
// passes the real hook, is pushed, and restores identically from a fresh
// clone. A small file stays one object.
func TestSealChunksAFileOverTheLimit(t *testing.T) {
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

	// Every pushed file is under the 50 MB GitHub warns about. The big file
	// is in chunks of at most 16 MiB, and the small one is a single object.
	var objects int
	for _, line := range strings.Split(strings.TrimSpace(e.must(b.remote, "git", "ls-tree", "-r", "-l", "main")), "\n") {
		fields := strings.Fields(line)
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			t.Fatalf("ls-tree line %q: %v", line, err)
		}
		if size > 16<<20+64<<10 {
			t.Errorf("pushed %s is %d bytes, more than a chunk", fields[4], size)
		}
		if strings.HasPrefix(fields[4], "objects/") {
			objects++
		}
	}
	if objects < 105/16+2 {
		t.Errorf("pushed %d objects, want at least %d chunks and 1 small file", objects, 105/16+1)
	}

	if out := e.must(b.base, "salt", "verify", b.dir); !strings.Contains(out, "All 2 files") {
		t.Fatalf("verify:\n%s", out)
	}
	if out := e.must(b.base, "salt", "doctor", b.dir); strings.Contains(out, "GitHub") {
		t.Fatalf("doctor warns about the chunks:\n%s", out)
	}
	e.must(b.base, "salt", "seal", "--prune", b.src, b.dir)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an unchanged chunked file changed the repo:\n%s", st)
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

// objectFiles lists the files under the repo's objects folder.
func objectFiles(t *testing.T, repo string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(repo, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out[p] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A live SQLite database larger than a chunk, changed by one row, adds only
// a few new chunks rather than a new copy of all of it. Each backup restores
// the database as it was.
func TestSealChunksAChangedDatabase(t *testing.T) {
	sqlite := realSQLite(t)
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	write(t, filepath.Join(b.src, "MEMORY.md"), "small\n")
	db := filepath.Join(b.base, "agent.db")
	e.must(b.base, sqlite, db, "PRAGMA journal_mode=WAL; CREATE TABLE m(id INTEGER PRIMARY KEY, body TEXT); "+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<40000) INSERT INTO m SELECT i, hex(randomblob(400)) FROM n; "+
		"CREATE INDEX mb ON m(body);")
	backup := func(subject string) {
		e.must(b.base, "salt", "seal", "--prune", "--sqlite", db, b.src, b.dir)
		e.must(b.dir, "git", "add", "-A")
		b.commitOn(t, "2026-09-01", subject)
	}
	backup("first")
	before := objectFiles(t, b.dir)
	e.must(b.base, sqlite, db, "UPDATE m SET body = 'changed' WHERE id = 20000;")
	backup("second")
	after := objectFiles(t, b.dir)
	added := 0
	for p := range after {
		if !before[p] {
			added++
		}
	}
	t.Logf("one changed row added %d of %d chunks", added, len(after)-1)
	if added == 0 || added > (len(after)-1)/2 {
		t.Fatalf("one changed row added %d of %d chunks", added, len(after)-1)
	}
	for _, rev := range []string{"HEAD~1", "HEAD"} {
		wt := filepath.Join(b.base, "wt-"+strings.ReplaceAll(rev, "~", "-"))
		e.must(b.dir, "git", "worktree", "add", "-q", "--detach", wt, rev)
		dest := filepath.Join(b.base, "restored-"+filepath.Base(wt))
		e.must(b.base, "salt", "restore", wt, "--to", dest)
		got := strings.TrimSpace(e.must(dest, sqlite, filepath.Join(dest, "agent.db"), "SELECT count(*), body = 'changed' FROM m WHERE id = 20000; PRAGMA integrity_check;"))
		want := map[string]string{"HEAD~1": "1|0\nok", "HEAD": "1|1\nok"}[rev]
		if got != want {
			t.Fatalf("%s restored %q, want %q", rev, got, want)
		}
	}
}
