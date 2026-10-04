package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/keys"
)

const backupURL = "https://ghp_secret@github.com/me/backup.git"

// remoteEnv is a sealed repo that a fake clone copies, as if downloaded,
// with the home folder in a temp dir so leftover downloads can be seen.
func remoteEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t)
	healthyRepo(t, e)
	e.app.Home = t.TempDir()
	e.git.clone = func(_ context.Context, dir string) error {
		return os.CopyFS(dir, os.DirFS(e.root))
	}
	return e
}

// noDownloadsLeft fails if a download folder is still in the home folder.
func noDownloadsLeft(t *testing.T, e *testEnv) {
	t.Helper()
	if entries, _ := os.ReadDir(e.app.Home); len(entries) != 0 {
		t.Fatalf("left in the home folder: %v", entries)
	}
}

func TestRestoreFromURL(t *testing.T) {
	// Named, not by URL: t.TempDir would put the token in the restore path.
	for name, url := range map[string]string{"https": backupURL, "ssh": "git@github.com:me/backup.git"} {
		t.Run(name, func(t *testing.T) {
			e := remoteEnv(t)
			var downloaded string
			e.git.clone = func(_ context.Context, dir string) error {
				downloaded = dir
				return os.CopyFS(dir, os.DirFS(e.root))
			}
			dest := filepath.Join(t.TempDir(), "restored")
			if err := e.app.Restore(RestoreOptions{Repo: url, To: dest, Paths: []string{"USER.md"}}); err != nil {
				t.Fatalf("Restore: %v\n%s", err, e.ui.out.String())
			}
			if b, err := os.ReadFile(filepath.Join(dest, "USER.md")); err != nil || string(b) != "hello" {
				t.Fatalf("restored USER.md = %q, %v", b, err)
			}
			if !slices.Equal(e.git.cloned, []string{url}) {
				t.Fatalf("cloned %q, want %q", e.git.cloned, url)
			}
			if filepath.Dir(downloaded) != e.app.Home || !strings.HasPrefix(filepath.Base(downloaded), "salt-download-") {
				t.Fatalf("downloaded into %s, want a salt-download- folder in %s", downloaded, e.app.Home)
			}
			out := e.ui.out.String()
			if !strings.Contains(out, "salt: downloading the latest backup from "+strings.Replace(url, "ghp_secret@", "", 1)+"\n") {
				t.Fatalf("no download message:\n%s", out)
			}
			if strings.Contains(out, "secret") {
				t.Fatalf("output shows the token:\n%s", out)
			}
			noDownloadsLeft(t, e)
		})
	}
}

func TestRestoreFromURLDownloadFails(t *testing.T) {
	e := remoteEnv(t)
	e.git.clone = func(_ context.Context, dir string) error {
		os.WriteFile(filepath.Join(dir, "partial"), nil, 0o600)
		return errors.New("git clone https://github.com/me/backup.git: exit status 128: fatal: repository not found")
	}
	dest := filepath.Join(t.TempDir(), "restored")
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: dest})
	if err == nil || !strings.Contains(err.Error(), "downloading the backup: git clone") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest created: %v", err)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLNotSalt(t *testing.T) {
	e := remoteEnv(t)
	e.git.clone = func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, "README.md"), []byte("not salt"), 0o644)
	}
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")})
	if err == nil || err.Error() != "https://github.com/me/backup.git is not a salt backup repo (it has no .salt/format.json)" {
		t.Fatalf("Restore: %v", err)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLInterrupted(t *testing.T) {
	e := remoteEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.git.clone = func(ctx context.Context, dir string) error {
		cancel()
		return ctx.Err()
	}
	dest := filepath.Join(t.TempDir(), "restored")
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: dest, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "the download was removed") {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest created: %v", err)
	}
	noDownloadsLeft(t, e)
}

// A restore that fails after the download, here for want of a key, still
// removes the download.
func TestRestoreFromURLRemovesTheDownloadOnFailure(t *testing.T) {
	e := remoteEnv(t)
	e.app.Store = &keys.MemStore{} // a new machine
	e.ui.interactive = false
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.Contains(err.Error(), "no key for this backup") {
		t.Fatalf("Restore: %v", err)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLWithoutADownloadFolder(t *testing.T) {
	e := remoteEnv(t)
	e.app.Home = filepath.Join(t.TempDir(), "file")
	os.WriteFile(e.app.Home, nil, 0o600)
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.Contains(err.Error(), "making a folder to download the backup into") {
		t.Fatalf("Restore: %v", err)
	}
	if len(e.git.cloned) != 0 {
		t.Fatalf("cloned without a folder: %q", e.git.cloned)
	}
}
