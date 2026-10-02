package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// MaxWorkers caps how many files are hashed or encrypted at once.
const MaxWorkers = 4

// Options configures Seal.
type Options struct {
	// CacheDir holds the change-detection cache. Required.
	CacheDir string
	// Prune removes everything in the repository that this seal did not
	// produce, apart from .git, .salt and the public files. Use it when the
	// repository should mirror the snapshot exactly.
	Prune bool
	// Exclude lists base names to skip in the source tree.
	Exclude []string
	// Workers overrides the worker count (1..MaxWorkers).
	Workers int
	// Show, if set, is how paths are written in messages, such as "~/…".
	Show func(path string) string
	// Extra lists files sealed as if they were in the source tree.
	Extra []Extra
}

// Extra is a file sealed as if it were at Rel in the source tree, such as a
// safe copy of a live database. Mode and ModTime are recorded instead of the
// file's own, so the backup keeps the original's.
type Extra struct {
	Rel     string // slash path in the backup
	Path    string // the file to read
	Mode    fs.FileMode
	ModTime time.Time
}

// ErrDuplicatePath means two files would be backed up at the same path, or a
// file at a path the source tree uses as a folder.
var ErrDuplicatePath = errors.New("two files would be backed up at the same path")

// show writes p for a message through fn, or unchanged when fn is nil.
func show(fn func(string) string, p string) string {
	if fn == nil {
		return p
	}
	return fn(p)
}

// DefaultExclude is skipped unless Options.Exclude is set.
var DefaultExclude = []string{".DS_Store", ".git"}

// Result summarises a seal.
type Result struct {
	Files     int      // regular files in the snapshot
	Symlinks  int      // symlinks recorded in the index
	Encrypted int      // files (re-)encrypted this run
	Reused    int      // unchanged files whose ciphertext was kept
	Removed   []string // repo paths deleted as stale or unmanaged
	Skipped   []string // source paths that are not files or symlinks
	IndexNew  bool     // whether index.age was rewritten
}

type item struct {
	rel     string // slash path relative to src
	abs     string
	mode    fs.FileMode
	modTime time.Time
	symlink string
	isLink  bool
}

// entriesHook lets tests change the entries before Seal checks them. It is
// always nil outside tests.
var entriesHook func([]Entry) []Entry

// Seal encrypts the tree at src into the repository r.
func Seal(src string, r *repo.Repo, opt Options) (*Result, error) {
	if opt.CacheDir == "" {
		return nil, errors.New("seal: no cache directory")
	}
	if err := checkDisjoint(src, r.Root, opt.Show); err != nil {
		return nil, err
	}
	if err := CheckNoSymlinks(r.Root); err != nil {
		return nil, err
	}
	exclude := opt.Exclude
	if exclude == nil {
		exclude = DefaultExclude
	}
	res := &Result{}
	items, err := walkSource(src, exclude, res, opt.Show)
	if err != nil {
		return nil, err
	}
	if items, err = addExtra(items, opt.Extra); err != nil {
		return nil, err
	}
	if len(items) > MaxIndexEntries {
		return nil, fmt.Errorf("%s has %d files; salt supports at most %d per backup", show(opt.Show, src), len(items), MaxIndexEntries)
	}

	// All repo writes go through rt, which refuses paths that lead outside
	// the repo (for example a symlinked objects/ folder).
	rt, err := os.OpenRoot(r.Root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()

	cPath, err := cachePath(opt.CacheDir, r.Root)
	if err != nil {
		return nil, err
	}
	c := loadCache(cPath, cacheKey(r.RecipientStrings, r.Format.EncryptPaths))

	entries := make([]Entry, len(items))
	newCache := make([]cacheEntry, len(items))
	var encrypted, reused atomic.Int64
	err = forEach(len(items), workers(opt.Workers), func(i int) error {
		it := items[i]
		e := Entry{Path: it.rel, Mode: uint32(it.mode.Perm())}
		if it.isLink {
			e.Symlink = it.symlink
			entries[i] = e
			return nil
		}
		sha, size, err := hashFile(it.abs)
		if err != nil {
			return err
		}
		e.MTime = unixNano(it.modTime)
		if prev, ok := c.Files[it.rel]; ok && prev.SHA256 == sha && objectIntact(rt, prev) {
			e.Object, e.SHA256, e.Size = prev.Object, sha, size
			entries[i], newCache[i] = e, prev
			reused.Add(1)
			return nil
		}
		obj, err := objectName(r.Format.EncryptPaths, it.rel)
		if err != nil {
			return err
		}
		ce, n, err := encryptFile(rt, r, it.abs, obj)
		if err != nil {
			return fmt.Errorf("%s: %w", it.rel, err)
		}
		e.Object, e.SHA256, e.Size = obj, ce.SHA256, n
		entries[i], newCache[i] = e, ce
		encrypted.Add(1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Encrypted, res.Reused = int(encrypted.Load()), int(reused.Load())

	next := &cache{Key: c.Key, Files: map[string]cacheEntry{}}
	keep := map[string]bool{}
	for i, e := range entries {
		if e.Symlink != "" || items[i].isLink {
			res.Symlinks++
			continue
		}
		res.Files++
		next.Files[e.Path] = newCache[i]
		keep[e.Object] = true
	}

	if entriesHook != nil {
		entries = entriesHook(entries)
	}
	if err := checkIndexEntries(entries); err != nil {
		return nil, err
	}
	ix := &Index{Version: repo.FormatVersion, Entries: entries}
	b, ixSHA, err := ix.marshal()
	if err != nil {
		return nil, err
	}
	if len(b) > maxIndexSize {
		return nil, fmt.Errorf("the index would be %d bytes; salt supports at most %d", len(b), maxIndexSize)
	}
	next.IndexSHA, next.IndexSize = ixSHA, c.IndexSize
	if ixSHA != c.IndexSHA || !sizeIs(rt, repo.IndexFile, c.IndexSize) {
		if _, err := encryptTo(rt, repo.IndexFile, bytes.NewReader(b), r.Recipients); err != nil {
			return nil, fmt.Errorf("writing index: %w", err)
		}
		fi, err := rt.Lstat(repo.IndexFile)
		if err != nil {
			return nil, err
		}
		next.IndexSize = fi.Size()
		res.IndexNew = true
	}

	removed, err := removeStale(rt, keep, opt.Prune)
	res.Removed = removed
	if err != nil {
		return res, err
	}
	return res, next.save(cPath)
}

func encryptFile(rt *os.Root, r *repo.Repo, abs, obj string) (cacheEntry, int64, error) {
	f, err := os.Open(abs)
	if err != nil {
		return cacheEntry{}, 0, err
	}
	defer f.Close()
	cr := &countReader{r: f}
	sha, err := encryptTo(rt, obj, cr, r.Recipients)
	if err != nil {
		return cacheEntry{}, 0, err
	}
	fi, err := rt.Lstat(filepath.FromSlash(obj))
	if err != nil {
		return cacheEntry{}, 0, err
	}
	return cacheEntry{SHA256: sha, Object: obj, CipherSize: fi.Size()}, cr.n, nil
}

// objectName picks where a file's ciphertext lives. With encrypted paths it
// is random, so the name reveals nothing about the file.
func objectName(encryptPaths bool, rel string) (string, error) {
	if !encryptPaths {
		return path.Join(repo.FilesDir, rel) + ".age", nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	h := hex.EncodeToString(b)
	return path.Join(repo.ObjectsDir, h[:2], h[2:]+".age"), nil
}

// Unix nanoseconds cover the years 1678 to 2262. unixNano returns 0 (not
// recorded) for a time outside them, where time.UnixNano is undefined.
var (
	minUnixNano = time.Unix(0, math.MinInt64)
	maxUnixNano = time.Unix(0, math.MaxInt64)
)

func unixNano(t time.Time) int64 {
	if t.Before(minUnixNano) || t.After(maxUnixNano) {
		return 0
	}
	return t.UnixNano()
}

func objectIntact(rt *os.Root, ce cacheEntry) bool {
	return ce.Object != "" && sizeIs(rt, ce.Object, ce.CipherSize)
}

// sizeIs reports whether rel is a regular file (not a symlink) of size n.
func sizeIs(rt *os.Root, rel string, n int64) bool {
	fi, err := rt.Lstat(filepath.FromSlash(rel))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == n
}

func walkSource(src string, exclude []string, res *Result, fn func(string) string) ([]item, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", show(fn, src))
	}
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	var items []item
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return nil
		}
		if skip[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			items = append(items, item{rel: rel, abs: p, symlink: target, isLink: true})
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			items = append(items, item{rel: rel, abs: p, mode: info.Mode(), modTime: info.ModTime()})
		default:
			res.Skipped = append(res.Skipped, rel)
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	return items, err
}

// addExtra adds the extra files to the source items, refusing a path that is
// unsafe or already taken by a file or folder.
func addExtra(items []item, extra []Extra) ([]item, error) {
	if len(extra) == 0 {
		return items, nil
	}
	files, dirs := map[string]bool{}, map[string]bool{}
	add := func(rel string) {
		files[rel] = true
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, it := range items {
		add(it.rel)
	}
	for _, x := range extra {
		rel, err := repo.CleanPath(x.Rel)
		if err != nil {
			return nil, err
		}
		clash := files[rel] || dirs[rel]
		for d := path.Dir(rel); d != "." && !clash; d = path.Dir(d) {
			clash = files[d]
		}
		if clash {
			return nil, fmt.Errorf("%w: %s", ErrDuplicatePath, rel)
		}
		add(rel)
		items = append(items, item{rel: rel, abs: x.Path, mode: x.Mode, modTime: x.ModTime})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	return items, nil
}

// removeStale deletes ciphertext no longer referenced by the index, leftover
// temp files, and (with prune) anything else salt does not manage.
func removeStale(rt *os.Root, keep map[string]bool, prune bool) ([]string, error) {
	var removed []string
	rfs := rt.FS()
	entries, err := fs.ReadDir(rfs, ".")
	if err != nil {
		return nil, err
	}
	for _, top := range entries {
		name := top.Name()
		switch {
		case name == ".git" || name == repo.Dir || name == repo.IndexFile || repo.Public[name]:
			continue
		case name == repo.ObjectsDir || name == repo.FilesDir:
			err := walkRepoDir(rt, name, func(rel string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				if keep[rel] {
					return nil
				}
				removed = append(removed, rel)
				return rt.Remove(filepath.FromSlash(rel))
			})
			if err != nil {
				return removed, err
			}
			removeEmptyDirs(rt, name)
		case strings.HasPrefix(name, ".salt-tmp-") || prune:
			removed = append(removed, name)
			if err := rt.RemoveAll(name); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

func removeEmptyDirs(rt *os.Root, dir string) {
	entries, err := fs.ReadDir(rt.FS(), dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyDirs(rt, path.Join(dir, e.Name()))
		}
	}
	rt.Remove(filepath.FromSlash(dir)) // fails, harmlessly, unless empty
}

// checkDisjoint refuses a source inside the repo or a repo inside the source:
// either would seal ciphertext into itself or leak plaintext into the repo.
func checkDisjoint(src, root string, fn func(string) string) error {
	a, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	b, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if within(a, b) || within(b, a) {
		return fmt.Errorf("source %s and repository %s must not contain each other", show(fn, a), show(fn, b))
	}
	return nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func workers(n int) int {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	return max(1, min(n, MaxWorkers))
}

// forEach runs fn for 0..n-1 on a bounded pool, stopping at the first error.
func forEach(n, workers int, fn func(i int) error) error {
	var (
		next     atomic.Int64
		firstErr error
		once     sync.Once
		failed   atomic.Bool
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					once.Do(func() { firstErr = err })
					failed.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

type countReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
