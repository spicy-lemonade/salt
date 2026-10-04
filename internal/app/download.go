package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// decrypted until the download is complete. A download left behind by an
// earlier one is pointed out.
//
// done must be called with the command's result once the repo is no longer
// needed. For a download, it removes the download, reporting it if it
// cannot, and returns err with the URL in place of the download's folder,
// which is gone by the time the message is read. For a folder it returns
// err as it is.
func (a *App) openRepo(ctx context.Context, spec string) (r *repo.Repo, done func(err error) error, err error) {
	remote, err := gitx.IsRemote(spec)
	if err != nil {
		return nil, nil, err
	}
	if !remote {
		r, err := repo.Open(spec)
		return r, func(err error) error { return err }, err
	}
	for _, d := range a.leftoverDownloads() {
		a.UI.Printf("salt: %s\n", leftoverDownload(a.short(d)))
	}
	tmp, err := os.MkdirTemp(a.downloadDir(), downloadPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("making a folder to download the backup into: %w", err)
	}
	shown := gitx.RedactURL(spec)
	named := strings.NewReplacer(tmp, shown, a.short(tmp), shown)
	done = func(err error) error {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			a.UI.Printf("salt: could not remove the download in %s (%v). It holds the encrypted backup and the URL it came from, so delete it yourself\n", a.short(tmp), rmErr)
		}
		if err == nil {
			return nil
		}
		return renamedError{err, named}
	}
	a.UI.Printf("salt: downloading the latest backup from %s\n", shown)
	if err := a.Git.Clone(ctx, spec, tmp); err != nil {
		if ctx.Err() != nil {
			err = errDownloadStopped
		} else {
			err = fmt.Errorf("downloading the backup: %w", err)
		}
		return nil, nil, done(err)
	}
	r, err = repo.Open(tmp)
	if errors.Is(err, repo.ErrNotInitialised) {
		err = fmt.Errorf("%s is not a salt backup repo (it has no %s)", shown, repo.FormatFile)
	}
	if err != nil {
		return nil, nil, done(err)
	}
	return r, done, nil
}

// renamedError is err with its message passed through a replacer.
type renamedError struct {
	err   error
	named *strings.Replacer
}

func (e renamedError) Error() string { return e.named.Replace(e.err.Error()) }

func (e renamedError) Unwrap() error { return e.err }

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
