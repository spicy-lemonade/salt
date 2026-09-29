package seal

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"sync"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// VerifyResult summarises a verify.
type VerifyResult struct {
	Files    int
	Symlinks int
	Bytes    int64
	// Problems are files that cannot be restored correctly.
	Problems []string
	// Unreferenced are ciphertext files the index does not mention. They do
	// not affect a restore; the next `salt seal` removes them.
	Unreferenced []string
}

// Verify proves every file in the backup can be restored: it decrypts each
// object, discarding the plaintext, and checks it against the index. Nothing
// is written to disk. Unlike Restore it reports every problem rather than
// stopping at the first.
func Verify(root string, ids []age.Identity, workerCount int) (*VerifyResult, error) {
	ix, err := ReadIndex(root, ids)
	if err != nil {
		return nil, err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	res := &VerifyResult{}
	var files []Entry
	referenced := map[string]bool{}
	for _, e := range ix.Entries {
		if e.Symlink != "" {
			res.Symlinks++
			continue
		}
		files = append(files, e)
		referenced[e.Object] = true
	}
	res.Files = len(files)

	var mu sync.Mutex
	forEach(len(files), workers(workerCount), func(i int) error {
		n, err := verifyEntry(rt, ids, files[i])
		mu.Lock()
		defer mu.Unlock()
		res.Bytes += n
		if err != nil {
			res.Problems = append(res.Problems, err.Error())
		}
		return nil // keep going: report everything
	})
	sort.Strings(res.Problems)

	for _, dir := range []string{repo.ObjectsDir, repo.FilesDir} {
		fs.WalkDir(rt.FS(), dir, func(rel string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !referenced[rel] {
				res.Unreferenced = append(res.Unreferenced, rel)
			}
			return nil
		})
	}
	return res, nil
}

func verifyEntry(rt *os.Root, ids []age.Identity, e Entry) (int64, error) {
	r, closeFn, err := decryptStream(rt, e.Object, ids)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%s: its encrypted file %s is missing", e.Path, e.Object)
	}
	if err != nil {
		return 0, fmt.Errorf("%s: %v", e.Path, err)
	}
	defer closeFn()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return n, fmt.Errorf("%s: cannot be decrypted (%v)", e.Path, err)
	}
	if n != e.Size || sum(h) != e.SHA256 {
		return n, fmt.Errorf("%s: content does not match the index", e.Path)
	}
	return n, nil
}
