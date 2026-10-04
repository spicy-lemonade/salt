//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remoteBackup pushes two backups to a bare repo and has git fetch the
// URLs https://example.test/NAME and git@example.test:NAME from it, through
// url.insteadOf in a git config of the test's own. Salt only sees the https
// or SSH URL; git fetches over file://, where --depth applies as it would on
// GitHub. It returns the folder holding the bare repos.
func remoteBackup(t *testing.T, e *env) string {
	t.Helper()
	base := t.TempDir()
	served := filepath.Join(base, "served")
	remote := filepath.Join(served, "backup.git")
	repoDir := filepath.Join(base, "backup")
	src := filepath.Join(base, "stage")
	cfg := filepath.Join(base, "gitconfig")
	write(t, cfg, "[url \"file://"+served+"/\"]\n\tinsteadOf = https://example.test/\n\tinsteadOf = git@example.test:\n")
	e.vars = append(e.vars, "GIT_CONFIG_GLOBAL="+cfg) // the last value is used
	e.must(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	// Keep every pushed object loose, so the test can delete one.
	e.must(remote, "git", "config", "receive.unpackLimit", "100000")
	e.must(base, "git", "clone", "-q", remote, repoDir)

	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", repoDir, "--recovery", "passphrase", "--passphrase-file", passFile)
	for _, content := range []string{"The user lives in Dublin.\n", "The user moved to Cork.\n"} {
		write(t, filepath.Join(src, "memories", "USER.md"), content)
		e.must(base, "salt", "seal", "--prune", src, repoDir)
		e.must(repoDir, "git", "add", "-A")
		e.must(repoDir, "git", "commit", "-q", "-m", "backup")
	}
	e.must(repoDir, "git", "push", "-q", "origin", "main")

	// Break the older backup on the remote. A full clone now fails, so a
	// restore that works proves only the latest backup was downloaded.
	old := strings.TrimSpace(e.must(repoDir, "git", "rev-parse", "HEAD~1"))
	if err := os.Remove(filepath.Join(remote, "objects", old[:2], old[2:])); err != nil {
		t.Fatalf("removing the older backup's commit: %v", err)
	}
	if out, code := e.run(base, "git", "clone", "-q", "https://example.test/backup.git", filepath.Join(base, "full")); code == 0 {
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
	for _, url := range []string{"https://example.test/backup.git", "git@example.test:backup.git"} {
		dest := filepath.Join(t.TempDir(), "restored")
		out := e.must(e.home, "salt", "restore", url, "--to", dest)
		if !strings.Contains(out, "salt: downloading the latest backup from "+url) {
			t.Fatalf("restore %s:\n%s", url, out)
		}
		if b, err := os.ReadFile(filepath.Join(dest, "memories", "USER.md")); err != nil || string(b) != "The user moved to Cork.\n" {
			t.Fatalf("restored USER.md from %s = %q, %v", url, b, err)
		}
		noDownloadsLeft(t, e)
	}
}

func TestRestoreFromURLFailures(t *testing.T) {
	e := newEnv(t)
	served := remoteBackup(t, e)
	e.must(served, "git", "init", "-q", "--bare", "-b", "main", "empty.git")

	for _, tt := range []struct {
		url, want string
	}{
		{"https://example.test/missing.git", "salt: downloading the backup: git clone https://example.test/missing.git"},
		{"https://example.test/empty.git", "salt: https://example.test/empty.git is not a salt backup repo (it has no .salt/format.json)"},
		// Nothing listens on port 1, so this fails at once.
		{"https://you:s3cret@127.0.0.1:1/backup.git", "salt: downloading the backup: git clone https://127.0.0.1:1/backup.git"},
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
