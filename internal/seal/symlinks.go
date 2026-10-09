package seal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// managed lists the repo paths salt reads and writes. Salt never creates a
// symlink at or below any of them (symlinks from the source are recorded in
// the encrypted index instead), so a symlink there was added by someone else.
var managed = []string{repo.Dir, repo.IndexFile, repo.ObjectsDir, repo.FilesDir}

// ErrForeignSymlink means the backup repo contains a symlink salt did not
// create.
var ErrForeignSymlink = repo.ErrForeignSymlink

// CheckNoSymlinks refuses a repo with any symlink where salt keeps its data,
// or at any of the extra repo paths given. os.Root already stops links that
// lead outside the repo; this also stops links that point back inside it, for
// example objects/ -> . , which would make seal's clean-up delete files from
// .git.
func CheckNoSymlinks(root string, extra ...string) error {
	for _, name := range append(managed[:len(managed):len(managed)], extra...) {
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
				return repo.ForeignSymlink(filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// walkRepoDir walks one of salt's folders (objects/ or files/) inside rt.
// fs.WalkDir follows a symlink at the top, so a folder that is a symlink is
// refused here too, not only by CheckNoSymlinks beforehand. A missing folder
// is nothing to walk.
func walkRepoDir(rt *os.Root, dir string, fn fs.WalkDirFunc) error {
	fi, err := rt.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return repo.ForeignSymlink(dir)
	}
	return fs.WalkDir(rt.FS(), dir, fn)
}
