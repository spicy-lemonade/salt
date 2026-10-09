//go:build e2e

package e2e

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// remoteBackup pushes two backups to a bare repo and has git fetch the
// URLs https://example.test/NAME, ssh://git@example.test/NAME and
// git@example.test:NAME from it, through
// url.insteadOf in a git config of the test's own. Salt only sees the https
// or SSH URL; git fetches over file://, where --depth applies as it would on
// GitHub. It returns the folder holding the bare repos.
func remoteBackup(t *testing.T, e *env) string {
	t.Helper()
	b := newBackupRepo(t, e)
	served := filepath.Join(b.base, "served")
	cfg := filepath.Join(b.base, "gitconfig")
	write(t, cfg, "[url \"file://"+served+"/\"]\n\tinsteadOf = https://example.test/\n\tinsteadOf = ssh://git@example.test/\n\tinsteadOf = git@example.test:\n")
	e.vars = append(e.vars, "GIT_CONFIG_GLOBAL="+cfg) // the last value is used
	remote := filepath.Join(served, "backup.git")
	if err := os.Mkdir(served, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(b.remote, remote); err != nil {
		t.Fatal(err)
	}
	b.remote = remote
	e.must(b.dir, "git", "remote", "set-url", "origin", remote)
	// Keep every pushed object loose, so the test can delete one.
	e.must(remote, "git", "config", "receive.unpackLimit", "100000")

	b.files["memories/USER.md"] = "The user lives in Dublin.\n"
	b.backup(t, "2026-09-01")
	b.files["memories/USER.md"] = "The user moved to Cork.\n"
	b.backup(t, "2026-09-02")
	e.must(b.dir, "git", "push", "-q", "origin", "main")

	// Break the older backup on the remote. A full clone now fails, so a
	// restore that works proves only the latest backup was downloaded.
	old := strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD~1"))
	if err := os.Remove(filepath.Join(remote, "objects", old[:2], old[2:])); err != nil {
		t.Fatalf("removing the older backup's commit: %v", err)
	}
	if out, code := e.run(b.base, "git", "clone", "-q", "https://example.test/backup.git", filepath.Join(b.base, "full")); code == 0 {
		t.Fatalf("a full clone worked without the older backup:\n%s", out)
	}
	return served
}

// noDownloadsLeft fails if a restore left its download in the home folder.
func noDownloadsLeft(t *testing.T, e *env) {
	t.Helper()
	if found, _ := filepath.Glob(filepath.Join(e.home, "salt-download-*")); len(found) > 0 {
		t.Fatalf("downloads left behind: %v", found)
	}
}

func TestRestoreFromURL(t *testing.T) {
	e := newEnv(t)
	remoteBackup(t, e)
	for url, shown := range map[string]string{
		"https://example.test/backup.git":   "https://example.test/backup.git",
		"ssh://git@example.test/backup.git": "ssh://example.test/backup.git",
		"git@example.test:backup.git":       "git@example.test:backup.git",
	} {
		dest := filepath.Join(t.TempDir(), "restored")
		out := e.must(e.home, "salt", "restore", url, "--to", dest)
		if !strings.Contains(out, "salt: downloading the latest backup from "+shown+"\n") {
			t.Fatalf("restore %s:\n%s", url, out)
		}
		if b, err := os.ReadFile(filepath.Join(dest, "memories", "USER.md")); err != nil || string(b) != "The user moved to Cork.\n" {
			t.Fatalf("restored USER.md from %s = %q, %v", url, b, err)
		}
		noDownloadsLeft(t, e)
	}
}

// A relative HOME gives a relative download folder, which git must not take
// as relative to itself once it runs there.
func TestRestoreFromURLWithARelativeHome(t *testing.T) {
	e := newEnv(t)
	remoteBackup(t, e)
	dest := filepath.Join(t.TempDir(), "restored")
	e.with("HOME="+filepath.Base(e.home)).must(filepath.Dir(e.home), "salt", "restore", "https://example.test/backup.git", "--to", dest)
	if b, err := os.ReadFile(filepath.Join(dest, "memories", "USER.md")); err != nil || string(b) != "The user moved to Cork.\n" {
		t.Fatalf("restored USER.md = %q, %v", b, err)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLFailures(t *testing.T) {
	e := newEnv(t)
	served := remoteBackup(t, e)
	e.must(served, "git", "init", "-q", "--bare", "-b", "main", "empty.git")

	for _, tt := range []struct {
		url, want string
	}{
		{"https://example.test/missing.git", "salt: downloading the backup: git fetch https://example.test/missing.git"},
		{"https://example.test/empty.git", "salt: https://example.test/empty.git is not a salt backup repo (its default branch has no .salt/format.json)"},
		{"http://you:s3cret@example.test/backup.git", "salt: salt cannot download a backup from http://example.test/backup.git."},
		// Nothing listens on port 1, so this fails at once.
		{"https://you:s3cret@127.0.0.1:1/backup.git", "salt: downloading the backup: git fetch https://127.0.0.1:1/backup.git"},
	} {
		dest := filepath.Join(t.TempDir(), "restored")
		out, code := e.run(e.home, "salt", "restore", tt.url, "--to", dest)
		if code != 1 || !strings.Contains(out, tt.want) {
			t.Fatalf("restore %s: exit %d, want %q in\n%s", tt.url, code, tt.want, out)
		}
		if strings.Contains(out, "s3cret") {
			t.Fatalf("restore %s shows the password:\n%s", tt.url, out)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("restore %s created %s: %v", tt.url, dest, err)
		}
		noDownloadsLeft(t, e)
	}
}

// startStuckDownload starts salt restore from an https URL, with the user
// name and password in userinfo if it is not empty, served by a server that
// takes the connection and never answers, as one on a network that has gone
// quiet does, and returns once git has connected. salt runs in
// a process group of its own, which is killed when the test ends, with
// anything git started.
func startStuckDownload(t *testing.T, e *env, userinfo string) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	conns := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			conns <- c
		}
	}()
	cmd := exec.Command(e.bin, "restore", "https://"+userinfo+ln.Addr().String()+"/backup.git", "--to", filepath.Join(t.TempDir(), "restored"))
	cmd.Dir = e.home
	cmd.Env = e.vars
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	select {
	case c := <-conns:
		t.Cleanup(func() { c.Close() })
	case <-time.After(10 * time.Second):
		t.Fatalf("git never connected:\n%s", out)
	}
	return cmd, out
}

// assertInterrupted checks that cmd, a restore that was stopped, exits as
// interrupted within 20 seconds and leaves no download behind.
func assertInterrupted(t *testing.T, e *env, cmd *exec.Cmd, out *strings.Builder) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("salt did not stop")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 || !strings.Contains(out.String(), "salt: restore interrupted") {
		t.Fatalf("exit %v, want 130:\n%s", err, out)
	}
	noDownloadsLeft(t, e)
}

// SIGTERM reaches salt alone, so git is stopped but git-remote-https, which
// git started, still holds git's error output open. salt must not wait for
// it.
func TestRestoreFromURLStopsAStuckDownload(t *testing.T) {
	e := newEnv(t)
	cmd, out := startStuckDownload(t, e, "")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	assertInterrupted(t, e, cmd, out)
}

// Ctrl-C reaches git as well as salt, and git can stop before salt has
// cancelled the download. Here only git gets SIGINT, so salt learns of it
// from git alone, and must still treat it as an interruption.
func TestRestoreFromURLGitInterruptedFirst(t *testing.T) {
	e := newEnv(t)
	cmd, out := startStuckDownload(t, e, "")
	pids := strings.Fields(e.must(e.home, "pgrep", "-P", strconv.Itoa(cmd.Process.Pid), "git"))
	if len(pids) != 1 {
		t.Fatalf("git processes under salt: %q", pids)
	}
	git, err := strconv.Atoi(pids[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(git, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	assertInterrupted(t, e, cmd, out)
}

// Ctrl-C in a terminal sends SIGINT to every program in its process group.
func TestRestoreFromURLCtrlC(t *testing.T) {
	e := newEnv(t)
	cmd, out := startStuckDownload(t, e, "")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	assertInterrupted(t, e, cmd, out)
}

// A download killed outright, part way, is left behind, but holds no
// password or token from the URL, since the URL is never saved in it.
func TestRestoreFromURLKilledLeavesNoToken(t *testing.T) {
	e := newEnv(t)
	cmd, _ := startStuckDownload(t, e, "you:s3cret-token@")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	found, _ := filepath.Glob(filepath.Join(e.home, "salt-download-*"))
	if len(found) != 1 {
		t.Fatalf("downloads left: %v", found)
	}
	err := filepath.WalkDir(found[0], func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), "s3cret-token") {
			t.Errorf("%s holds the token:\n%s", p, b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Without git, a restore from a URL says what is missing and leaves nothing
// behind.
func TestRestoreFromURLWithoutGit(t *testing.T) {
	e := newEnv(t)
	dest := filepath.Join(t.TempDir(), "restored")
	// Only salt's own folder is on PATH, so git cannot be found.
	out, code := e.with("PATH="+filepath.Dir(e.bin)).run(e.home, "salt", "restore", "https://example.test/backup.git", "--to", dest)
	if code != 1 || !strings.Contains(out, "salt: downloading the backup: salt needs the git program, which is not installed or not on PATH\n") {
		t.Fatalf("restore without git: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("restore without git created %s: %v", dest, err)
	}
	noDownloadsLeft(t, e)
}
