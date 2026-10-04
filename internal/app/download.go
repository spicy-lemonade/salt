package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// errDownloadStopped means ctx was cancelled while a backup was downloading.
// The download has been removed.
var errDownloadStopped = fmt.Errorf("the download was stopped: %w", context.Canceled)

// openRepo opens the backup repo that spec names: a folder, or a URL (see
// gitx.IsRemote). From a URL only the latest backup is downloaded, still
// encrypted, into a private folder in the home folder, so nothing is
// decrypted until the download is complete. done removes the download once
// the repo is no longer needed, and does nothing for a folder.
func (a *App) openRepo(ctx context.Context, spec string) (r *repo.Repo, done func(), err error) {
	if !gitx.IsRemote(spec) {
		r, err := repo.Open(spec)
		return r, func() {}, err
	}
	tmp, err := os.MkdirTemp(a.Home, "salt-download-")
	if err != nil {
		return nil, nil, fmt.Errorf("making a folder to download the backup into: %w", err)
	}
	done = func() { os.RemoveAll(tmp) }
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
