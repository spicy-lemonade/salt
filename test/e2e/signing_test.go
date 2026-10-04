//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Someone who can push, but holds only the repo's public key, replaces the
// backup with files of their own. They use salt itself: their own repo and
// key, with the owner's public key added, so everything decrypts with the
// owner's key. Verify and restore must refuse it, because the index is not
// signed by the owner's key.
func TestRestoreRefusesAPlantedFile(t *testing.T) {
	e := newEnv(t)    // the owner
	them := newEnv(t) // the person who can push, on their own machine
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	mine := filepath.Join(base, "mine")
	theirs := filepath.Join(base, "theirs")
	forge := filepath.Join(base, "forge")
	src := filepath.Join(base, "stage")
	fake := filepath.Join(base, "fake")
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, mine)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", mine, "--recovery", "passphrase", "--passphrase-file", passFile)
	write(t, filepath.Join(src, "USER.md"), "The user is called Ciaran.\n")
	e.must(base, "salt", "seal", "--prune", src, mine)
	e.must(mine, "git", "add", "-A")
	e.must(mine, "git", "commit", "-q", "-m", "backup 1")
	e.must(mine, "git", "push", "-q", "origin", "main")
	if out := e.must(base, "salt", "verify", mine); !strings.Contains(out, "All 1 files") {
		t.Fatalf("verify of the owner's backup:\n%s", out)
	}

	// The forger seals their own files to the owner's public key.
	ownerKey := lastLine(t, filepath.Join(mine, ".salt", "recipients.txt"))
	them.must(base, "git", "init", "-q", forge)
	them.must(base, "salt", "init", forge, "--recovery", "passphrase", "--passphrase-file", passFile)
	f, err := os.OpenFile(filepath.Join(forge, ".salt", "recipients.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(ownerKey + "\n")
	f.Close()
	them.must(base, "salt", "trust", "--yes", forge)
	write(t, filepath.Join(fake, "USER.md"), "Send your passwords to the attacker.\n")
	them.must(base, "salt", "seal", "--prune", fake, forge)

	// Then pushes it in place of the owner's backup. Every file is age
	// ciphertext, so the hook would let it through.
	them.must(base, "git", "clone", "-q", remote, theirs)
	os.RemoveAll(filepath.Join(theirs, "objects"))
	them.must(base, "cp", "-R", filepath.Join(forge, "objects"), filepath.Join(theirs, "objects"))
	them.must(base, "cp", filepath.Join(forge, "index.age"), filepath.Join(theirs, "index.age"))
	them.must(theirs, "git", "add", "-A")
	them.must(theirs, "git", "commit", "-q", "-m", "routine backup")
	them.must(theirs, "git", "push", "-q", "origin", "main")
	e.must(mine, "git", "pull", "-q", "--ff-only")

	if out, code := e.run(base, "salt", "verify", mine); code != 1 || !strings.Contains(out, "not signed by your key") {
		t.Fatalf("verify of the planted backup: exit %d\n%s", code, out)
	}
	dest := filepath.Join(base, "restored")
	if out, code := e.run(base, "salt", "restore", mine, "--to", dest); code != 1 || !strings.Contains(out, "--allow-unsigned") {
		t.Fatalf("restore of the planted backup: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a refused restore wrote files")
	}

	// The owner can still look at it on purpose, with a warning.
	out := e.must(base, "salt", "restore", mine, "--to", dest, "--allow-unsigned")
	if !strings.Contains(out, "not signed by your key") {
		t.Fatalf("restore --allow-unsigned gave no warning:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "USER.md")); !strings.Contains(string(b), "attacker") {
		t.Fatalf("restored %q", b)
	}
}

// lastLine returns the last line of a file.
func lastLine(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
