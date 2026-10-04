package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// downloadPattern names the folders backups are downloaded into.
const downloadPattern = "salt-download-*"

// errDownloadStopped means ctx was cancelled while a backup was downloading.
var errDownloadStopped = fmt.Errorf("the download was stopped: %w", context.Canceled)

// openRepo opens the backup repo that spec names: a folder, or a URL (see
// gitx.IsRemote). From a URL only the latest backup is downloaded, still
// encrypted, into a private folder in the home folder, so nothing is
// decrypted until the download is complete. done removes the download once
// the repo is no longer needed, and does nothing for a folder. A download
// that cannot be removed is reported, and so is one an earlier download
// left behind.
func (a *App) openRepo(ctx context.Context, spec string) (r *repo.Repo, done func(), err error) {
	remote, err := gitx.IsRemote(spec)
	if err != nil {
		return nil, nil, err
	}
	if !remote {
		r, err := repo.Open(spec)
		return r, func() {}, err
	}
	for _, d := range a.leftoverDownloads() {
		a.UI.Printf("salt: %s\n", leftoverDownload(a.short(d)))
	}
	tmp, err := os.MkdirTemp(a.downloadDir(), downloadPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("making a folder to download the backup into: %w", err)
	}
	done = func() {
		if err := os.RemoveAll(tmp); err != nil {
			a.UI.Printf("salt: could not remove the download in %s (%v). It holds the encrypted backup and the URL it came from, so delete it yourself\n", a.short(tmp), err)
		}
	}
	shown := gitx.RedactURL(spec)
	a.UI.Printf("salt: downloading the latest backup from %s\n", shown)
	if err := a.Git.Clone(ctx, spec, tmp); err != nil {
		done()
		if ctx.Err() != nil {
			return nil, nil, errDownloadStopped
		}
		return nil, nil, fmt.Errorf("downloading the backup: %w", err)
	}
	r, err = repo.Open(tmp)
	if err != nil {
		done()
		if errors.Is(err, repo.ErrNotInitialised) {
			return nil, nil, fmt.Errorf("%s is not a salt backup repo (it has no %s)", shown, repo.FormatFile)
		}
		return nil, nil, err
	}
	return r, done, nil
}

// downloadDir is where backups are downloaded: the home folder, so a large
// backup does not fill a temp folder kept in memory, or the temp folder when
// there is no home folder.
func (a *App) downloadDir() string {
	if a.Home == "" {
		return os.TempDir()
	}
	return a.Home
}

// leftoverDownloads lists downloads that were never removed, because salt
// was killed outright (SIGKILL, a power cut) or could not remove them.
func (a *App) leftoverDownloads() []string {
	found, _ := filepath.Glob(filepath.Join(a.downloadDir(), downloadPattern))
	return found
}

func leftoverDownload(dir string) string {
	return fmt.Sprintf("%s was left by a download that did not finish. It holds only encrypted files and the URL they came from, so delete it once you have checked it", dir)
}

func (a *App) doctorDownloads(r *report) {
	for _, d := range a.leftoverDownloads() {
		r.add(warn, "%s", leftoverDownload(a.short(d)))
	}
}
