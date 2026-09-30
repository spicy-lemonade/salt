//go:build e2e

// Package e2e runs the real salt binary against real git. Run it with
// `make e2e`, which builds salt once, caps the number of processes, and sets
// the variables checked below. The tests never build salt themselves, and
// every salt they start uses SALT_KEYSTORE=file with a temporary HOME, so the
// real keychain is never touched.
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
	if os.Getenv("SALT_E2E") != "1" {
		os.Stderr.WriteString("e2e tests only run through `make e2e`\n")
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
	e := &env{t: t, bin: bin, home: home, vars: []string{
		"PATH=" + filepath.Dir(bin) + ":/usr/local/go/bin:/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"SALT_KEYSTORE=file", // never the real keychain
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}}
	// A coverage-recording salt writes its data here (see make e2e).
	if d := os.Getenv("GOCOVERDIR"); d != "" {
		e.vars = append(e.vars, "GOCOVERDIR="+d)
	}
	return e
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

	// doctor and verify are happy with a real repo.
	if out := e.must(base, "salt", "doctor", repoDir); !strings.Contains(out, "✓ last commit contains no unencrypted files") {
		t.Fatalf("doctor:\n%s", out)
	}
	if out := e.must(base, "salt", "verify", repoDir); !strings.Contains(out, "All 2 files") {
		t.Fatalf("verify:\n%s", out)
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
	// doctor spots the plaintext left in the working tree.
	if out, code := e.run(base, "salt", "doctor", repoDir); code != 1 || !strings.Contains(out, "leak.md") {
		t.Fatalf("doctor with plaintext: exit %d\n%s", code, out)
	}
	os.Remove(filepath.Join(repoDir, "leak.md"))

	// Restore from a fresh clone on a "new machine": no saved key, so salt
	// asks for the passphrase on stdin.
	e.forgetKeys()
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

// forgetKeys deletes every saved key, simulating a new machine.
func (e *env) forgetKeys() {
	e.t.Helper()
	filepath.WalkDir(e.home, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "keys" && filepath.Base(filepath.Dir(p)) == "salt" {
			os.RemoveAll(p)
			return filepath.SkipDir
		}
		return nil
	})
}

// The hook calls `salt` by name; if salt is missing, the commit is refused.
func TestHookFailsClosedWithoutSalt(t *testing.T) {
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"} {
		if _, err := os.Stat(filepath.Join(d, "salt")); err == nil {
			t.Skipf("a salt is installed in %s, which the hook always searches", d)
		}
	}
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

// Git edge cases: no repo, no commits, no remote, odd staged entries.
func TestGitEdgeCases(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()

	// Outside any git repo, check and hook install fail with a clear error.
	plain := filepath.Join(base, "plain")
	os.MkdirAll(plain, 0o755)
	if out, code := e.run(plain, "salt", "check"); code != 1 || !strings.Contains(out, "refusing it") {
		t.Fatalf("check outside git: exit %d\n%s", code, out)
	}
	if out, code := e.run(plain, "salt", "hook", "install"); code != 1 {
		t.Fatalf("hook install outside git: exit %d\n%s", code, out)
	}

	// A fresh repo: no commits, no remote.
	repoDir := filepath.Join(base, "fresh")
	e.must(base, "git", "init", "-q", "-b", "main", repoDir)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", repoDir, "--recovery", "passphrase", "--passphrase-file", passFile, "--plain-paths")
	out, _ := e.run(base, "salt", "doctor", repoDir)
	for _, want := range []string{"no commits yet", "no `origin` remote", "no backup sealed yet", "file paths visible"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor on a fresh repo missing %q:\n%s", want, out)
		}
	}

	// A file name with a newline, and a submodule entry, are both refused.
	write(t, filepath.Join(repoDir, "odd\nname.md"), "x")
	e.must(repoDir, "git", "add", "odd\nname.md")
	e.must(repoDir, "git", "update-index", "--add", "--cacheinfo",
		"160000,1111111111111111111111111111111111111111,sub")
	out, code := e.run(repoDir, "salt", "check")
	if code != 1 || !strings.Contains(out, "2 staged file(s)") || !strings.Contains(out, "not a regular file") {
		t.Fatalf("check with odd entries: exit %d\n%s", code, out)
	}
}

// Exit codes: 2 for usage mistakes, 1 for failures.
func TestExitCodes(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"bogus"}, 2},
		{[]string{"restore", "repo"}, 2},
		{[]string{"verify", filepath.Join(e.home, "missing")}, 1},
		{[]string{"help"}, 0},
	} {
		if out, code := e.run(e.home, "salt", tc.args...); code != tc.code {
			t.Errorf("salt %v: exit %d, want %d\n%s", tc.args, code, tc.code, out)
		}
	}
}

// A plaintext file named "0:x" must not hide behind an encrypted "x":
// git reads ":0:x" as stage 0 of "x".
func TestCheckStageNameTrick(t *testing.T) {
	e := newEnv(t)
	repoDir := filepath.Join(t.TempDir(), "r")
	e.must(filepath.Dir(repoDir), "git", "init", "-q", "-b", "main", repoDir)
	write(t, filepath.Join(repoDir, "x"), "age-encryption.org/v1\n-> X25519 fake\n")
	write(t, filepath.Join(repoDir, "0:x"), "secret plaintext")
	e.must(repoDir, "git", "add", "x", "0:x")
	out, code := e.run(repoDir, "salt", "check")
	if code != 1 || !strings.Contains(out, "0:x: not encrypted") {
		t.Fatalf("check: exit %d\n%s", code, out)
	}
}

// Someone with push access adds their own key to the repo. After a pull,
// seal must refuse until the owner approves the change with `salt trust`.
func TestSealRefusesAKeyAddedByAnotherPusher(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	mine := filepath.Join(base, "mine")
	theirs := filepath.Join(base, "theirs")
	src := filepath.Join(base, "stage")
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, mine)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", mine, "--recovery", "passphrase", "--passphrase-file", passFile)
	write(t, filepath.Join(src, "USER.md"), "secret\n")
	e.must(base, "salt", "seal", "--prune", src, mine)
	e.must(mine, "git", "add", "-A")
	e.must(mine, "git", "commit", "-q", "-m", "backup 1")
	e.must(mine, "git", "push", "-q", "origin", "main")

	// The attacker adds their key from another clone. The hook doesn't stop
	// them: recipients.txt is a public file.
	e.must(base, "git", "clone", "-q", remote, theirs)
	f, _ := os.OpenFile(filepath.Join(theirs, ".salt", "recipients.txt"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\n")
	f.Close()
	e.must(theirs, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-am", "totally normal change")
	e.must(theirs, "git", "push", "-q", "origin", "main")

	e.must(mine, "git", "pull", "-q", "--ff-only")
	write(t, filepath.Join(src, "USER.md"), "new secret\n")
	out, code := e.run(base, "salt", "seal", "--prune", src, mine)
	if code != 1 || !strings.Contains(out, "key added: age1qyqszqgpqyqszqgp") {
		t.Fatalf("seal after the attacker's commit: exit %d\n%s", code, out)
	}
	if out, code := e.run(base, "salt", "doctor", mine); code != 1 || !strings.Contains(out, "changed since you approved") {
		t.Fatalf("doctor: exit %d\n%s", code, out)
	}
	// Once the owner has checked and approved the change, sealing works.
	e.must(base, "salt", "trust", "--yes", mine)
	e.must(base, "salt", "seal", "--prune", src, mine)
}

// Someone with push access replaces objects/ with a symlink back to the repo
// itself. Seal once "cleaned up" .git through it. It must refuse instead.
func TestSealRefusesAPushedSymlink(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	mine := filepath.Join(base, "mine")
	theirs := filepath.Join(base, "theirs")
	src := filepath.Join(base, "stage")
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, mine)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", mine, "--recovery", "passphrase", "--passphrase-file", passFile)
	write(t, filepath.Join(src, "USER.md"), "secret\n")
	e.must(base, "salt", "seal", "--prune", src, mine)
	e.must(mine, "git", "add", "-A")
	e.must(mine, "git", "commit", "-q", "-m", "backup 1")
	e.must(mine, "git", "push", "-q", "origin", "main")

	e.must(base, "git", "clone", "-q", remote, theirs)
	e.must(theirs, "git", "rm", "-rq", "objects")
	if err := os.Symlink(".", filepath.Join(theirs, "objects")); err != nil {
		t.Fatal(err)
	}
	e.must(theirs, "git", "add", "objects")
	e.must(theirs, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "tidy up")
	e.must(theirs, "git", "push", "-q", "origin", "main")

	e.must(mine, "git", "pull", "-q", "--ff-only")
	out, code := e.run(base, "salt", "seal", "--prune", src, mine)
	if code != 1 || !strings.Contains(out, "symlink salt did not create at objects") {
		t.Fatalf("seal after the pushed symlink: exit %d\n%s", code, out)
	}
	// The local repo is intact.
	e.must(mine, "git", "status", "--short")
	if _, err := os.Stat(filepath.Join(mine, ".salt", "format.json")); err != nil {
		t.Fatal(".salt/format.json was deleted")
	}
	if out, code := e.run(base, "salt", "verify", mine); code != 1 || !strings.Contains(out, "symlink") {
		t.Fatalf("verify: exit %d\n%s", code, out)
	}
}

// Someone who can push before the owner sets up salt commits .salt as a link
// to a folder outside the repo. Init in a fresh clone must refuse and write
// nothing there.
func TestInitRefusesASymlinkedSaltFolder(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	theirs := filepath.Join(base, "theirs")
	mine := filepath.Join(base, "mine")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o755)
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, theirs)
	if err := os.Symlink("../outside", filepath.Join(theirs, ".salt")); err != nil {
		t.Fatal(err)
	}
	e.must(theirs, "git", "add", ".salt")
	e.must(theirs, "git", "commit", "-q", "-m", "set things up")
	e.must(theirs, "git", "push", "-q", "origin", "main")

	e.must(base, "git", "clone", "-q", remote, mine)
	if fi, err := os.Lstat(filepath.Join(mine, ".salt")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("clone has no .salt symlink: %v", err)
	}
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	out, code := e.run(base, "salt", "init", mine, "--recovery", "passphrase", "--passphrase-file", passFile)
	if code == 0 || !strings.Contains(out, "symlink salt did not create at .salt") {
		t.Fatalf("init with a symlinked .salt: exit %d\n%s", code, out)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("init wrote outside the repo: %v", entries)
	}
}

// pushedChange sets up a backed-up repo, lets someone else push a change to
// one public file from their own clone, and pulls it into the owner's clone.
// It returns the owner's clone and the snapshot directory.
func pushedChange(t *testing.T, e *env, file, line string) (mine, src string) {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	mine = filepath.Join(base, "mine")
	theirs := filepath.Join(base, "theirs")
	src = filepath.Join(base, "stage")
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	e.must(base, "git", "clone", "-q", remote, mine)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", mine, "--recovery", "passphrase", "--passphrase-file", passFile)
	write(t, filepath.Join(src, "USER.md"), "secret\n")
	e.must(base, "salt", "seal", "--prune", src, mine)
	e.must(mine, "git", "add", "-A")
	e.must(mine, "git", "commit", "-q", "-m", "backup 1")
	e.must(mine, "git", "push", "-q", "origin", "main")

	// salt check allows the change: both files are on the public list.
	e.must(base, "git", "clone", "-q", remote, theirs)
	f, err := os.OpenFile(filepath.Join(theirs, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line + "\n")
	f.Close()
	e.must(theirs, "git", "add", file)
	e.must(theirs, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "tidy up")
	e.must(theirs, "git", "push", "-q", "origin", "main")
	e.must(mine, "git", "pull", "-q", "--ff-only")
	return mine, src
}

func commitCount(e *env, dir string) string {
	return strings.TrimSpace(e.must(dir, "git", "rev-list", "--count", "HEAD"))
}

// A pushed "objects/" ignore rule would make `git add -A` skip every new
// object, so the remote gets an index pointing at files it never receives.
// The next seal must fail before the script commits anything.
func TestPushedIgnoreRuleStopsTheBackup(t *testing.T) {
	e := newEnv(t)
	mine, src := pushedChange(t, e, ".gitignore", "objects/")
	before := commitCount(e, mine)

	write(t, filepath.Join(src, "USER.md"), "new secret\n")
	out, code := e.run(mine, "salt", "seal", "--prune", src, mine)
	if code != 1 || !strings.Contains(out, "is ignored by git (via .gitignore or your git config), so it would never reach the remote. Remove the matching ignore rule.") {
		t.Fatalf("seal after the ignore rule: exit %d\n%s", code, out)
	}
	if after := commitCount(e, mine); after != before {
		t.Fatalf("commits went from %s to %s", before, after)
	}
	// A script that carried on anyway is stopped by the hook.
	e.must(mine, "git", "add", "-A")
	if out, code := e.run(mine, "git", "commit", "-q", "-m", "backup 2"); code == 0 || !strings.Contains(out, "salt check: refusing commit") {
		t.Fatalf("commit after the ignore rule: exit %d\n%s", code, out)
	}
	if out, code := e.run(mine, "salt", "doctor", mine); code != 1 || !strings.Contains(out, "is ignored by git") {
		t.Fatalf("doctor: exit %d\n%s", code, out)
	}
}

// A pushed "*.age text" line overrides "*.age binary", so git would rewrite
// line endings in ciphertext it stores, and a fresh clone would rewrite the
// objects already committed. Seal must fail even on a night with no changes.
func TestPushedAttributeStopsTheBackup(t *testing.T) {
	e := newEnv(t)
	mine, src := pushedChange(t, e, ".gitattributes", "*.age text eol=crlf")

	out, code := e.run(mine, "salt", "seal", "--prune", src, mine)
	if code != 1 || !strings.Contains(out, "when storing it (text is set, eol is set to crlf); backups could not be restored. Remove the attribute from .gitattributes (or your git config) so *.age stays binary.") {
		t.Fatalf("unchanged seal after the attribute: exit %d\n%s", code, out)
	}
	// One line per file, and the count in the first line counts files:
	// index.age, .salt/key.age and the one object.
	if n := strings.Count(out, "  git would change "); n != 3 || !strings.Contains(out, "but 3 file(s) would not reach the remote intact") {
		t.Fatalf("want 3 files on 3 lines, got %d lines:\n%s", n, out)
	}
	// The hook refuses any commit, even one that stages nothing salt wrote.
	if out, code := e.run(mine, "git", "commit", "-q", "--allow-empty", "-m", "backup 2"); code == 0 || !strings.Contains(out, "salt check: refusing commit") {
		t.Fatalf("commit after the attribute: exit %d\n%s", code, out)
	}

	// A changed file: seal deletes the old object and writes a new one. git
	// still lists the deleted one until it is staged, but it never reaches
	// the remote, so seal and the hook count the same 3 files.
	write(t, filepath.Join(src, "USER.md"), "new secret\n")
	out, code = e.run(mine, "salt", "seal", "--prune", src, mine)
	if n := strings.Count(out, "  git would change "); code != 1 || n != 3 || !strings.Contains(out, "but 3 file(s) would not reach the remote intact") {
		t.Fatalf("changed seal after the attribute: exit %d, %d lines\n%s", code, n, out)
	}
	e.must(mine, "git", "add", "-A")
	if out, code := e.run(mine, "git", "commit", "-q", "-m", "backup 3"); code == 0 || !strings.Contains(out, "salt check: refusing commit: 3 file(s) would not reach the remote intact") {
		t.Fatalf("commit of the changed backup: exit %d\n%s", code, out)
	}
	if out, code := e.run(mine, "salt", "doctor", mine); code != 1 || !strings.Contains(out, "git would change") {
		t.Fatalf("doctor: exit %d\n%s", code, out)
	}
}

// hook install shows a hook inside the home folder as ~/….
func TestHookInstallShowsHomePath(t *testing.T) {
	e := newEnv(t)
	repoDir := filepath.Join(e.home, "backup")
	e.must(e.home, "git", "init", "-q", "-b", "main", repoDir)
	out := e.must(e.home, "salt", "hook", "install", repoDir)
	if want := "✓ Installed ~/backup/.git/hooks/pre-commit"; !strings.Contains(out, want) {
		t.Fatalf("hook install: want %q in:\n%s", want, out)
	}
}
