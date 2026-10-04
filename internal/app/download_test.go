package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
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
	for _, tt := range []struct{ name, url, shown string }{
		{"https", backupURL, "https://github.com/me/backup.git"},
		{"ssh", "ssh://git@github.com/me/backup.git", "ssh://github.com/me/backup.git"},
		{"ssh form", "git@github.com:me/backup.git", "git@github.com:me/backup.git"},
	} {
		url := tt.url
		t.Run(tt.name, func(t *testing.T) {
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
			if !strings.Contains(out, "salt: downloading the latest backup from "+tt.shown+"\n") {
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
	if err == nil || err.Error() != "https://github.com/me/backup.git is not a salt backup repo (its default branch has no .salt/format.json)" {
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
	if !errors.Is(err, ErrInterrupted) || err.Error() != "restore interrupted: "+e.app.short(dest)+" was not changed" {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest created: %v", err)
	}
	noDownloadsLeft(t, e)
}

// Ctrl-C can stop git before salt cancels its context. git's
// context.Canceled still counts as an interruption.
func TestRestoreFromURLGitStoppedFirst(t *testing.T) {
	e := remoteEnv(t)
	e.git.clone = func(context.Context, string) error { return context.Canceled }
	dest := filepath.Join(t.TempDir(), "restored")
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: dest, Context: context.Background()})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Restore: %v", err)
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

func TestOpenRepo(t *testing.T) {
	e := remoteEnv(t)
	r, done, err := e.app.openRepo(context.Background(), e.root)
	if err != nil || r.Root != e.root {
		t.Fatalf("openRepo(folder) = %v, %v", r, err)
	}
	if err := done(nil); err != nil {
		t.Fatalf("done = %v", err)
	}
	if _, err := os.Stat(e.root); err != nil || len(e.git.cloned) != 0 {
		t.Fatalf("a folder was downloaded or removed: %v, %q", err, e.git.cloned)
	}

	r, done, err = e.app.openRepo(context.Background(), backupURL)
	if err != nil || filepath.Dir(r.Root) != e.app.Home {
		t.Fatalf("openRepo(URL) = %v, %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "index.age")); err != nil {
		t.Fatalf("the download is not usable before done: %v", err)
	}
	if err := done(nil); err != nil {
		t.Fatalf("done = %v", err)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLRefusesOtherSchemes(t *testing.T) {
	e := remoteEnv(t)
	err := e.app.Restore(RestoreOptions{Repo: "http://ghp_secret@github.com/me/backup.git", To: filepath.Join(t.TempDir(), "r")})
	if err == nil || !strings.HasPrefix(err.Error(), "salt cannot download a backup from http://github.com/me/backup.git.") {
		t.Fatalf("Restore: %v", err)
	}
	if len(e.git.cloned) != 0 {
		t.Fatalf("cloned %q", e.git.cloned)
	}
	noDownloadsLeft(t, e)
}

func TestRestoreFromURLPointsOutLeftovers(t *testing.T) {
	e := remoteEnv(t)
	old := filepath.Join(e.app.Home, "salt-download-123")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := "salt: " + e.app.short(old) + " was left by a download that did not finish. It holds only encrypted files and the URL they came from, so delete it once you have checked it\n"
	if out := e.ui.out.String(); !strings.HasPrefix(out, want) {
		t.Fatalf("no warning about %s:\n%s", old, out)
	}
	// It is pointed out, never removed.
	if entries, _ := os.ReadDir(e.app.Home); len(entries) != 1 || entries[0].Name() != "salt-download-123" {
		t.Fatalf("left in the home folder: %v", entries)
	}
}

func TestRestoreFromURLCannotRemoveTheDownload(t *testing.T) {
	e := remoteEnv(t)
	var locked string
	e.git.clone = func(_ context.Context, dir string) error {
		if err := os.CopyFS(dir, os.DirFS(e.root)); err != nil {
			return err
		}
		// A folder whose files cannot be deleted stops the removal.
		locked = filepath.Join(dir, "locked")
		os.Mkdir(locked, 0o700)
		os.WriteFile(filepath.Join(locked, "f"), nil, 0o600)
		return os.Chmod(locked, 0o500)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := "salt: could not remove the download in " + e.app.short(filepath.Dir(locked)) + " ("
	if out := e.ui.out.String(); !strings.Contains(out, want) || !strings.Contains(out, "so delete it yourself\n") {
		t.Fatalf("no warning that the download is still there:\n%s", out)
	}
}

func TestDoctorPointsOutLeftoverDownloads(t *testing.T) {
	e := remoteEnv(t)
	old := filepath.Join(e.app.Home, "salt-download-123")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	e.app.Doctor(e.root)
	if out := e.ui.out.String(); !strings.Contains(out, "! "+e.app.short(old)+" was left by a download that did not finish.") {
		t.Fatalf("doctor did not point out %s:\n%s", old, out)
	}
}

func TestDownloadDir(t *testing.T) {
	a := &App{Home: "/home/you"}
	if got := a.downloadDir(); got != "/home/you" {
		t.Fatalf("downloadDir = %q", got)
	}
	a.Home = ""
	if got := a.downloadDir(); got != os.TempDir() {
		t.Fatalf("downloadDir without a home folder = %q, want %q", got, os.TempDir())
	}
}

// The download is gone by the time an error is read, so errors name the URL
// in its place.
func TestRestoreFromURLErrorsNameTheURL(t *testing.T) {
	e := remoteEnv(t)
	e.git.clone = func(_ context.Context, dir string) error {
		if err := os.CopyFS(dir, os.DirFS(e.root)); err != nil {
			return err
		}
		return os.Remove(filepath.Join(dir, repo.RecipientsFile))
	}
	err := e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")})
	want := "open https://github.com/me/backup.git/" + repo.RecipientsFile + ": no such file or directory"
	if err == nil || err.Error() != want || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Restore: %v, want %q", err, want)
	}
	noDownloadsLeft(t, e)

	// So do errors from after the repo is opened: here a passphrase backup
	// with no key file, on a machine without its key.
	e.git.clone = func(_ context.Context, dir string) error {
		if err := os.CopyFS(dir, os.DirFS(e.root)); err != nil {
			return err
		}
		p := filepath.Join(dir, repo.FormatFile)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(p, []byte(strings.Replace(string(b), `"recovery": "phrase"`, `"recovery": "passphrase"`, 1)), 0o644)
	}
	e.app.Store = &keys.MemStore{}
	err = e.app.Restore(RestoreOptions{Repo: backupURL, To: filepath.Join(t.TempDir(), "r")})
	want = "reading " + repo.KeyFile + ": open https://github.com/me/backup.git/" + repo.KeyFile + ": no such file or directory"
	if err == nil || err.Error() != want {
		t.Fatalf("Restore: %v, want %q", err, want)
	}
	noDownloadsLeft(t, e)
}

func TestDoneKeepsAFolderError(t *testing.T) {
	e := remoteEnv(t)
	_, done, err := e.app.openRepo(context.Background(), e.root)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("x " + e.root)
	if got := done(want); got != want {
		t.Fatalf("done = %v", got)
	}
}
