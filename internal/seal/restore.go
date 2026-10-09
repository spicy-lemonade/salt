package seal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// Paths limits the restore to these files or directories (slash paths as
	// in the index). Empty restores everything.
	Paths []string
	// Force allows restoring over an existing non-empty destination. The old
	// destination is moved aside, never deleted.
	Force   bool
	Workers int
	// Context stops the restore when it is cancelled, and the partly
	// restored files are removed. Nil runs the restore to the end.
	Context context.Context
	// Track, if set, is told the temporary directory as soon as it exists.
	// It returns a function Restore calls once that directory is gone, either
	// removed or moved into place. A directory that could not be removed is
	// never reported gone, so the caller can tell the person about it.
	Track func(tmp string) (done func())
	// Show, if set, is how paths are written in messages, such as "~/…".
	Show func(path string) string
	// SignatureOptions says which signatures on the index are accepted
	// (see ReadIndex).
	SignatureOptions
}

// RestoreResult summarises a restore.
type RestoreResult struct {
	Files    int
	Symlinks int
	// Unsigned means the index was not signed by one of the keys, and was
	// accepted only because of AllowUnsigned.
	Unsigned bool
	// MovedAside is where an existing destination was moved, if any.
	MovedAside string
}

// Restore decrypts the repository at root into dest. It decrypts into a
// temporary directory next to dest, verifies every file against the index,
// and only then moves the result into place.
func Restore(root string, ids []age.Identity, dest string, opt RestoreOptions) (*RestoreResult, error) {
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
	entries, err := filterEntries(ix.Entries, opt.Paths)
	if err != nil {
		return nil, err
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	existing, err := nonEmpty(dest)
	if err != nil {
		return nil, err
	}
	if existing && !opt.Force {
		return nil, fmt.Errorf("%s already exists and is not empty; choose an empty destination or pass --force (the existing directory is moved aside, not deleted)", show(opt.Show, dest))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".salt-restore-*")
	if err != nil {
		return nil, err
	}
	done := func() {}
	if opt.Track != nil {
		done = opt.Track(tmp)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
		if _, err := os.Lstat(tmp); errors.Is(err, fs.ErrNotExist) {
			done()
		}
	}()
	ctx := opt.Context
	if ctx == nil {
		ctx = context.Background()
	}

	res := &RestoreResult{Unsigned: ix.Unsigned}
	var files, links []Entry
	for _, e := range entries {
		if e.Symlink != "" {
			links = append(links, e)
		} else {
			files = append(files, e)
		}
	}
	err = forEach(len(files), workers(opt.Workers), func(i int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return restoreFile(ctx, rt, ids, tmp, files[i])
	})
	if err != nil {
		return nil, err
	}
	// Symlinks last, so no file is ever written through one.
	for _, e := range links {
		if err := safeParent(tmp, e.Path); err != nil {
			return nil, err
		}
		p := filepath.Join(tmp, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.Symlink(e.Symlink, p); err != nil {
			return nil, err
		}
	}
	res.Files, res.Symlinks = len(files), len(links)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if existing {
		res.MovedAside = fmt.Sprintf("%s.salt-old-%s", dest, time.Now().Format("20060102-150405"))
		if err := os.Rename(dest, res.MovedAside); err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(dest); err == nil {
		if err := os.Remove(dest); err != nil { // empty directory
			return nil, err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, err
	}
	ok = true
	return res, nil
}

func restoreFile(ctx context.Context, rt *os.Root, ids []age.Identity, tmp string, e Entry) error {
	if err := safeParent(tmp, e.Path); err != nil {
		return err
	}
	dst := filepath.Join(tmp, filepath.FromSlash(e.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	r, closeFn, err := decryptStream(rt, e.objects(), ids)
	if err != nil {
		return fmt.Errorf("%s: %w", clip(e.Path), err)
	}
	defer closeFn()
	// Owner-only permissions, keeping the owner's execute bit.
	perm := os.FileMode(e.Mode)&0o700 | 0o600
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	h := sha256.New()
	// One byte past the signed size is enough to tell the object is too
	// long, so a small object that expands hugely is never written out whole.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, r}, e.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", clip(e.Path), err)
	}
	if n != e.Size || sum(h) != e.SHA256 {
		return fmt.Errorf("%s: restored content does not match the index (corrupted backup?)", clip(e.Path))
	}
	return restoreModTime(dst, e)
}

// chtimes is os.Chtimes, replaced in tests to make it fail.
var chtimes = os.Chtimes

// restoreModTime gives a restored file its recorded last-modified date. An
// entry without one (an older backup) keeps the time of the restore. The
// access time is left alone.
func restoreModTime(dst string, e Entry) error {
	if e.MTime == 0 {
		return nil
	}
	if err := chtimes(dst, time.Time{}, time.Unix(0, e.MTime)); err != nil {
		return fmt.Errorf("%s: setting its last-modified date: %w", clip(e.Path), err)
	}
	return nil
}

// ctxReader stops reading once ctx is cancelled, so a large file does not
// have to finish before an interrupted restore cleans up.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// safeParent refuses to write below a symlink inside the restore directory.
func safeParent(base, rel string) error {
	parts := strings.Split(rel, "/")
	p := base
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: parent %s is a symlink; refusing to restore through it", clip(rel), clip(part))
		}
	}
	return nil
}

func filterEntries(all []Entry, paths []string) ([]Entry, error) {
	if len(paths) == 0 {
		return all, nil
	}
	var out []Entry
	matched := make([]bool, len(paths))
	for _, e := range all {
		for i, p := range paths {
			p = strings.Trim(p, "/")
			if e.Path == p || strings.HasPrefix(e.Path, p+"/") {
				out = append(out, e)
				matched[i] = true
				break
			}
		}
	}
	for i, m := range matched {
		if !m {
			return nil, fmt.Errorf("%s is not in this backup", paths[i])
		}
	}
	return out, nil
}

func nonEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return true, nil
	}
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return len(names) > 0, err
}
