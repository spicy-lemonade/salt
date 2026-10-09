package seal

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	Workers int
	// SignatureOptions says which signatures on the index are accepted
	// (see ReadIndex).
	SignatureOptions
}

// VerifyResult summarises a verify.
type VerifyResult struct {
	Files    int
	Symlinks int
	Bytes    int64
	// Unsigned means the index was not signed by one of the keys, and was
	// accepted only because of AllowUnsigned.
	Unsigned bool
	// Unapproved is the key that signed it, if one of the keys signed it
	// but SignedBy does not list it (see Index.Unapproved).
	Unapproved string
	// Problems describe files that cannot be restored correctly: the
	// MaxProblems of them first by path, in path order, so the list is the
	// same on every run. ProblemCount counts them all.
	Problems     []string
	ProblemCount int
	// Unreferenced are ciphertext files the index does not mention. They do
	// not affect a restore; the next `salt seal` removes them.
	Unreferenced []string
}

// MaxProblems is how many problems Verify describes in full. A tampered
// index can name 100,000 missing files, and listing them all would flood the
// terminal or cron mail.
const MaxProblems = 50

// Verify proves every file in the backup can be restored: it decrypts each
// object, discarding the plaintext, and checks it against the index. Nothing
// is written to disk. Unlike Restore it reports every problem rather than
// stopping at the first. An index that is not signed by one of the keys is
// refused outright, as Restore refuses it.
func Verify(root string, ids []age.Identity, opt VerifyOptions) (*VerifyResult, error) {
	if err := CheckNoSymlinks(root); err != nil {
		return nil, err
	}
	ix, err := ReadIndex(root, ids, opt.SignatureOptions)
	if err != nil {
		return nil, err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	res := &VerifyResult{Unsigned: ix.Unsigned, Unapproved: ix.Unapproved}
	var files []Entry
	referenced := map[string]bool{}
	for _, e := range ix.Entries {
		if e.Symlink != "" {
			res.Symlinks++
			continue
		}
		files = append(files, e)
		for _, obj := range e.objects() {
			referenced[obj] = true
		}
	}
	res.Files = len(files)

	// Only the first MaxProblems by path are kept, in order, so memory stays
	// bounded however many files fail.
	type problem struct{ path, msg string }
	var kept []problem
	var mu sync.Mutex
	forEach(len(files), workers(opt.Workers), func(i int) error {
		n, err := verifyEntry(rt, ids, files[i])
		mu.Lock()
		defer mu.Unlock()
		res.Bytes += n
		if err != nil {
			res.ProblemCount++
			p := problem{files[i].Path, err.Error()}
			at, _ := slices.BinarySearchFunc(kept, p.path, func(k problem, path string) int { return strings.Compare(k.path, path) })
			if at < MaxProblems {
				kept = slices.Insert(kept, at, p)
				kept = kept[:min(len(kept), MaxProblems)]
			}
		}
		return nil // keep going: report everything
	})
	for _, p := range kept {
		res.Problems = append(res.Problems, p.msg)
	}

	// Listing unreferenced files is informational, so a folder that can't be
	// walked (CheckNoSymlinks has already refused a symlinked one) is skipped.
	for _, dir := range []string{repo.ObjectsDir, repo.FilesDir} {
		walkRepoDir(rt, dir, func(rel string, d fs.DirEntry, err error) error {
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
	// Parts after the first are opened only as they are reached, so check
	// they are all there first, to name the missing one.
	objects := e.objects()
	for _, obj := range objects {
		if _, err := rt.Lstat(filepath.FromSlash(obj)); errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%s: its encrypted file %s is missing", clip(e.Path), clip(obj))
		}
	}
	r, closeFn, err := decryptStream(rt, objects, ids)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", clip(e.Path), err)
	}
	defer closeFn()
	h := sha256.New()
	// One byte past the signed size is enough to tell the object is too long.
	n, err := io.Copy(h, io.LimitReader(r, e.Size+1))
	if err != nil {
		return n, fmt.Errorf("%s: cannot be decrypted (%v)", clip(e.Path), err)
	}
	if n != e.Size || sum(h) != e.SHA256 {
		return n, fmt.Errorf("%s: content does not match the index", clip(e.Path))
	}
	return n, nil
}
