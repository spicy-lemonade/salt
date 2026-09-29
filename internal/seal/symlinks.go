package seal

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// managed lists the repo paths salt reads and writes. Salt never creates a
// symlink at or below any of them (symlinks from the source are recorded in
// the encrypted index instead), so a symlink there was added by someone else.
var managed = []string{repo.Dir, repo.IndexFile, repo.ObjectsDir, repo.FilesDir}

// ErrForeignSymlink means the backup repo contains a symlink salt did not
// create.
var ErrForeignSymlink = errors.New("backup repo contains a symlink salt did not create")

// checkNoSymlinks refuses a repo with any symlink where salt keeps its data.
// os.Root already stops links that lead outside the repo; this also stops
// links that point back inside it, for example objects/ -> . , which would
// make seal's clean-up delete files from .git.
func checkNoSymlinks(root string) error {
	for _, name := range managed {
		top := filepath.Join(root, name)
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) && p == top {
				return nil
			}
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				rel, _ := filepath.Rel(root, p)
				return fmt.Errorf("%w at %s. Someone else added it. Remove it and check the repo's recent commits before backing up or restoring",
					ErrForeignSymlink, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
