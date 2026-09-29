//go:build e2e

// Package e2e runs the real salt binary against real git. Run it with
// `make e2e`, which builds salt once and runs these tests inside a memory-
// and pid-capped container. The tests never build salt themselves, and they
// refuse to run outside the container because `salt init` writes to the
// system keychain.
package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SALT_E2E_CONTAINER") != "1" {
		os.Stderr.WriteString("e2e tests only run inside the test container: use `make e2e`\n")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type env struct {
	t    *testing.T
	bin  string
	home string
	vars []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	bin := os.Getenv("SALT_BIN")
	if bin == "" || !filepath.IsAbs(bin) {
		t.Fatalf("SALT_BIN must be an absolute path to a prebuilt salt, got %q", bin)
	}
	home := t.TempDir()
	return &env{t: t, bin: bin, home: home, vars: []string{
		"PATH=" + filepath.Dir(bin) + ":/usr/local/go/bin:/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}}
}

// run runs a command and returns combined output and exit code.
func (e *env) run(dir, name string, args ...string) (string, int) {
	e.t.Helper()
	// exec resolves names on this process's PATH, not cmd.Env's.
	if name == "salt" {
		name = e.bin
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.vars
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	}
	e.t.Fatalf("%s %v: %v", name, args, err)
	return "", -1
}

func (e *env) must(dir, name string, args ...string) string {
	e.t.Helper()
	out, code := e.run(dir, name, args...)
	if code != 0 {
		e.t.Fatalf("%s %v: exit %d\n%s", name, args, code, out)
	}
	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	if out := e.must(e.home, "salt", "version"); !strings.HasPrefix(out, "salt ") {
		t.Fatalf("version output %q", out)
	}
}

func TestNestedSaltRefused(t *testing.T) {
	e := newEnv(t)
	e.vars = append(e.vars, "SALT_ACTIVE=1")
	out, code := e.run(e.home, "salt", "version")
	if code != 3 || !strings.Contains(out, "refusing to start a nested salt") {
		t.Fatalf("nested salt: exit %d, output %q", code, out)
	}
}

// The full nightly flow with the real hook: init, seal, commit (hook passes),
// a plaintext commit (hook refuses), then restore from a fresh clone.
func TestBackupFlow(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	repoDir := filepath.Join(base, "backup")
	src := filepath.Join(base, "stage")
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, repoDir)

	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", repoDir, "--recovery", "passphrase", "--passphrase-file", passFile)
	e.must(repoDir, "git", "add", ".salt", ".gitattributes")
	e.must(repoDir, "git", "commit", "-q", "-m", "Set up salt") // hook runs and passes

	write(t, filepath.Join(src, "memories", "USER.md"), "The user is called Ciaran.\n")
	write(t, filepath.Join(src, "skills", "tax-return-2026", "SKILL.md"), "# Tax\n")
	e.must(base, "salt", "seal", "--prune", src, repoDir)
	e.must(repoDir, "git", "add", "-A")
	e.must(repoDir, "git", "commit", "-q", "-m", "backup 1")
	e.must(repoDir, "git", "push", "-q", "origin", "main")

	// Nothing plaintext was pushed, not even names.
	tree := e.must(repoDir, "git", "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, "USER") || strings.Contains(tree, "tax-return") {
		t.Fatalf("pushed tree reveals paths:\n%s", tree)
	}
	grep, _ := e.run(repoDir, "git", "grep", "-l", "Ciaran", "HEAD")
	if strings.TrimSpace(grep) != "" {
		t.Fatalf("plaintext found in commit: %s", grep)
	}

	// An unchanged snapshot produces no changes to commit.
	e.must(base, "salt", "seal", "--prune", src, repoDir)
	if st := e.must(repoDir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("unchanged snapshot changed the repo:\n%s", st)
	}

	// The hook refuses plaintext.
	write(t, filepath.Join(repoDir, "leak.md"), "The user is called Ciaran.\n")
	e.must(repoDir, "git", "add", "leak.md")
	out, code := e.run(repoDir, "git", "commit", "-q", "-m", "leak")
	if code == 0 || !strings.Contains(out, "leak.md: not encrypted") {
		t.Fatalf("plaintext commit: exit %d\n%s", code, out)
	}
	e.must(repoDir, "git", "reset", "-q", "HEAD", "leak.md")
	os.Remove(filepath.Join(repoDir, "leak.md"))

	// Restore from a fresh clone on a "new machine": no saved key, so salt
	// asks for the passphrase on stdin.
	e.must(base, "rm", "-rf", filepath.Join(e.home, ".config", "salt"))
	clone := filepath.Join(base, "clone")
	e.must(base, "git", "clone", "-q", remote, clone)
	dest := filepath.Join(base, "restored")
	cmd := exec.Command(e.bin, "restore", clone, "--to", dest)
	cmd.Env = e.vars
	cmd.Stdin = strings.NewReader("correct horse battery staple\n")
	// Not a terminal, so salt must refuse rather than prompt.
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no key for this backup") {
		t.Fatalf("restore without a key or terminal: %v\n%s", err, out)
	}
}

// The hook calls `salt` by name; if salt is missing, the commit is refused.
func TestHookFailsClosedWithoutSalt(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	repoDir := filepath.Join(base, "backup")
	e.must(base, "git", "init", "-q", "-b", "main", repoDir)
	e.must(repoDir, "salt", "hook", "install")
	write(t, filepath.Join(repoDir, "x.md"), "plaintext")
	e.must(repoDir, "git", "add", "x.md")

	noSalt := append([]string{}, e.vars...)
	noSalt[0] = "PATH=/usr/bin:/bin"
	cmd := exec.Command("git", "commit", "-q", "-m", "x")
	cmd.Dir = repoDir
	cmd.Env = noSalt
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "salt: not found on PATH") {
		t.Fatalf("commit without salt on PATH: %v\n%s", err, out)
	}
}
