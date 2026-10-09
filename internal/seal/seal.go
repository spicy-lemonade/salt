package seal

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
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
	"unicode"

	"github.com/spicy-lemonade/salt/internal/regular"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// MaxWorkers caps how many files are hashed or encrypted at once.
const MaxWorkers = 4

// A file is one object, unless it is over maxChunk bytes, when it is sealed
// in chunks (see chunk.go). zstd and age add about 0.03% to data that does
// not compress, so an object stays far below GitHub's limit. Should a live
// file grow past oneObjectLimit after it was measured, whatever runs past it
// goes into a second part. The 512 KiB left free covers age's 16 bytes per
// 64 KiB many times over.
//
// It is a variable so tests can split small files.
var oneObjectLimit int64 = repo.GitHubFileLimit - 512<<10

// Options configures Seal.
type Options struct {
	// CacheDir holds the change-detection cache. Required.
	CacheDir string
	// Signer signs the index (see keys.SigningKey). Required.
	Signer ed25519.PrivateKey
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
	// Live is true for a file another program may change or delete at any
	// time, such as one a preset found. Its permissions and date are then
	// read just before its contents, and Mode and ModTime are not used. One
	// deleted, or that is no longer a file, before it is read is left out of
	// the backup and listed in Result.Gone, instead of failing the seal.
	Live bool
	// SHA256, if set, is the hex SHA-256 the contents must have, such as
	// that of the contents checked for secrets. A file read with other
	// contents, changed since, is left out of the backup and listed in
	// Result.Changed, and whatever was written for it is removed.
	SHA256 string
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
	Files     int       // regular files in the snapshot
	Symlinks  int       // symlinks recorded in the index
	Encrypted int       // files (re-)encrypted this run
	Reused    int       // unchanged files whose ciphertext was kept
	Removed   []string  // repo paths deleted as stale or unmanaged
	Skipped   []string  // source paths that are not files or symlinks
	Gone      []string  // backup paths of live extra files deleted, or no longer files, before they were read
	Changed   []string  // backup paths of extra files whose contents were not Extra.SHA256
	Written   []Written // files that wrote new ciphertext, in path order
	IndexNew  bool      // whether index.age was rewritten
}

// Written is how many bytes of new ciphertext a seal wrote for one file.
type Written struct {
	Path  string // slash path in the backup
	Bytes int64
}

type item struct {
	rel     string // slash path relative to src
	abs     string
	mode    fs.FileMode
	modTime time.Time
	symlink string
	isLink  bool
	live    bool   // see Extra.Live
	sha256  string // see Extra.SHA256
}

// entriesHook lets tests change the entries before Seal checks them,
// hashedHook lets them change a file after Seal has measured it, and
// chunkHook lets them fail a seal before it writes a new chunk, given how
// many it has written for the file. All are always nil outside tests.
var (
	entriesHook func([]Entry) []Entry
	hashedHook  func(path string)
	chunkHook   func(written int) error
)

// Seal encrypts the tree at src into the repository r. An empty src seals
// only opt.Extra.
func Seal(src string, r *repo.Repo, opt Options) (*Result, error) {
	if opt.CacheDir == "" {
		return nil, errors.New("seal: no cache directory")
	}
	if len(opt.Signer) != ed25519.PrivateKeySize {
		return nil, errors.New("seal: no signing key")
	}
	if src != "" {
		if err := CheckDisjoint(src, r.Root, opt.Show); err != nil {
			return nil, err
		}
	}
	if err := CheckNoSymlinks(r.Root); err != nil {
		return nil, err
	}
	exclude := opt.Exclude
	if exclude == nil {
		exclude = DefaultExclude
	}
	res := &Result{}
	var items []item
	var err error
	if src != "" {
		if items, err = walkSource(src, exclude, res, opt.Show); err != nil {
			return nil, err
		}
	}
	if items, err = addExtra(items, opt.Extra, !r.Format.EncryptPaths); err != nil {
		return nil, err
	}
	if len(items) > MaxIndexEntries {
		what := "the backup"
		if src != "" {
			what = show(opt.Show, src)
		}
		return nil, fmt.Errorf("%s has %d files; salt supports at most %d per backup", what, len(items), MaxIndexEntries)
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
	left := make([]leftOut, len(items))
	var encrypted, reused atomic.Int64
	written := make([]int64, len(items))
	done := make([]bool, len(items)) // regular files fully sealed
	known := c.chunks()
	gear := chunkGear(opt.Signer.Seed())
	err = forEach(len(items), workers(opt.Workers), func(i int) error {
		var err error // each worker's own, never Seal's
		it := items[i]
		e := Entry{Path: it.rel, Mode: uint32(it.mode.Perm())}
		if it.isLink {
			e.Symlink = it.symlink
			entries[i] = e
			return nil
		}
		// A live file deleted since it was listed is left out, and so is one
		// no longer a file when it is opened. Only the file itself being gone
		// counts, never a path missing in the repo.
		vanished := func(err error) bool {
			if !it.live {
				return false
			}
			switch {
			case errors.Is(err, regular.ErrNotRegular):
				left[i] = gone
			case errors.Is(err, fs.ErrNotExist):
				if _, statErr := os.Lstat(it.abs); errors.Is(statErr, fs.ErrNotExist) {
					left[i] = gone
				}
			}
			return left[i] == gone
		}
		// A live file's permissions and date are read now, just before its
		// contents, as the tool may have changed it since it was found. One
		// that is no longer a file is left out, as a deleted one is.
		if it.live {
			fi, err := os.Stat(it.abs)
			if vanished(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				left[i] = gone
				return nil
			}
			it.mode, it.modTime = fi.Mode(), fi.ModTime()
			e.Mode = uint32(it.mode.Perm())
		}
		e.MTime = unixNano(it.modTime)
		prev, cached := c.Files[it.rel]
		ce, ok := prev, false
		var size int64
		// A file sealed in chunks last time, and still over maxChunk, is
		// chunked again at once: one read both hashes it and finds which
		// chunks changed. Any other file is hashed first, so an unchanged one
		// keeps its objects, and one that shrank is one object again.
		// Every error below falls through to one check, so a live file
		// deleted at any step is left out, and any other error names it.
		chunk := cached && prev.chunked()
		if chunk {
			var fi os.FileInfo
			if fi, err = os.Stat(it.abs); err == nil {
				chunk = fi.Size() > int64(maxChunk)
			}
		}
		if err == nil && !chunk {
			var sha string
			if sha, size, err = hashFile(it.abs); err == nil {
				if hashedHook != nil {
					hashedHook(it.abs)
				}
				if cached && prev.SHA256 == sha {
					ce, ok = objectIntact(rt, prev)
				}
				chunk = !ok && size > int64(maxChunk)
			}
		}
		var newBytes int64
		switch {
		case err != nil:
		case chunk:
			ce, size, newBytes, err = encryptChunks(rt, r, it.abs, gear, prev, known)
		case !ok:
			var obj string
			if obj, err = objectName(r.Format.EncryptPaths, it.rel); err == nil {
				ce, size, err = encryptFile(rt, r, it.abs, obj, oneObjectLimit)
				newBytes = ce.cipherSize()
			}
		}
		if vanished(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.rel, err)
		}
		// What it wrote is not in the index, so removeStale removes it.
		if it.sha256 != "" && ce.SHA256 != it.sha256 {
			left[i] = changed
			return nil
		}
		// A changed file whose chunks were all sealed before, such as one put
		// back as it was, wrote nothing new but is not unchanged.
		if newBytes == 0 && cached && ce.SHA256 == prev.SHA256 {
			reused.Add(1)
		} else {
			encrypted.Add(1)
		}
		written[i] = newBytes
		e.SHA256, e.Size = ce.SHA256, size
		for j, p := range ce.all() {
			if j == 0 {
				e.Object = p.Object
			} else {
				e.Parts = append(e.Parts, p.Object)
			}
		}
		entries[i], newCache[i] = e, ce
		done[i] = true
		return nil
	})
	if err != nil {
		// The files sealed before the failure keep what they wrote, so the
		// next seal reuses it rather than encrypting it all again. It also
		// records what a plain-paths object replaced in place now holds, so
		// it is never kept for the content it held before. Nothing is
		// removed; the next seal removes what no index needs. Losing this
		// save only costs that reuse, so the seal's own error is returned.
		// The cache is copied whole, so the index's entry keeps every field.
		// A file left out because it changed may have had its plain-paths
		// object replaced in place, so its entry is dropped.
		partial := *c
		partial.Files = maps.Clone(c.Files)
		for i, d := range done {
			switch {
			case d:
				partial.Files[items[i].rel] = newCache[i]
			case left[i] == changed:
				delete(partial.Files, items[i].rel)
			}
		}
		partial.save(cPath)
		return nil, err
	}
	res.Encrypted, res.Reused = int(encrypted.Load()), int(reused.Load())
	for i, n := range written {
		if n > 0 {
			res.Written = append(res.Written, Written{Path: items[i].rel, Bytes: n})
		}
	}
	items, entries, newCache, res.Gone, res.Changed = dropLeftOut(items, entries, newCache, left)

	next := &cache{Key: c.Key, Files: map[string]cacheEntry{}}
	keep := map[string]bool{}
	// An index with a file in parts or chunks gets a later version, so an
	// older salt that cannot read it refuses it.
	version := repo.FormatVersion
	for i, e := range entries {
		if e.Symlink != "" || items[i].isLink {
			res.Symlinks++
			continue
		}
		res.Files++
		next.Files[e.Path] = newCache[i]
		for _, obj := range e.objects() {
			keep[obj] = true
		}
		if len(e.Parts) > 0 {
			version = partsIndexVersion
		}
	}

	if entriesHook != nil {
		entries = entriesHook(entries)
	}
	if err := checkIndexEntries(entries); err != nil {
		return nil, err
	}
	ix := &Index{Version: version, Entries: entries}
	ix.sign(opt.Signer)
	b, ixSHA, err := ix.marshal()
	if err != nil {
		return nil, err
	}
	if len(b) > maxIndexSize {
		return nil, fmt.Errorf("the index would be %d bytes; salt supports at most %d", len(b), maxIndexSize)
	}
	next.IndexSHA = ixSHA
	ixPart, ok := partIntact(rt, cachePart{Object: repo.IndexFile, CipherSize: c.IndexSize, ModTime: c.IndexModTime})
	if ixSHA != c.IndexSHA || !ok {
		_, parts, err := encryptTo(rt, repo.IndexFile, bytes.NewReader(b), r.Recipients, 0)
		if err != nil {
			return nil, fmt.Errorf("writing index: %w", err)
		}
		ixPart = parts[0]
		res.IndexNew = true
	}
	next.IndexSize, next.IndexModTime = ixPart.CipherSize, ixPart.ModTime

	removed, err := removeStale(rt, keep, opt.Prune)
	res.Removed = removed
	if err != nil {
		return res, err
	}
	return res, next.save(cPath)
}

// leftOut is why a file was left out of the backup, if it was.
type leftOut uint8

const (
	kept    leftOut = iota
	gone            // see Result.Gone
	changed         // see Result.Changed
)

// dropLeftOut removes the items left out, with their entries and cache
// entries, keeping the rest in order, and returns the paths of those gone
// and of those changed.
func dropLeftOut(items []item, entries []Entry, newCache []cacheEntry, left []leftOut) ([]item, []Entry, []cacheEntry, []string, []string) {
	var paths [3][]string
	n := 0
	for i := range items {
		if left[i] != kept {
			paths[left[i]] = append(paths[left[i]], items[i].rel)
			continue
		}
		items[n], entries[n], newCache[n] = items[i], entries[i], newCache[i]
		n++
	}
	return items[:n], entries[:n], newCache[:n], paths[gone], paths[changed]
}

// encryptFile seals the file at abs into obj, in parts of limit compressed
// bytes if limit is above zero. It returns the plaintext size it read.
func encryptFile(rt *os.Root, r *repo.Repo, abs, obj string, limit int64) (cacheEntry, int64, error) {
	f, _, err := regular.Open(os.OpenFile, abs)
	if err != nil {
		return cacheEntry{}, 0, err
	}
	defer f.Close()
	cr := &countReader{r: f}
	sha, parts, err := encryptTo(rt, obj, cr, r.Recipients, limit)
	if err != nil {
		return cacheEntry{}, 0, err
	}
	return newCacheEntry(sha, parts), cr.n, nil
}

// encryptChunks seals the file at abs in chunks (see chunker). A chunk that
// was in the file last time (prev), met earlier in the file, or in known
// keeps its object if it is still intact; any other is compressed and
// encrypted on its own into a new object under a random name, even with
// plain paths, as a chunk can be in more than one file. The file's own
// chunks are looked up first, so an unchanged file always keeps its own
// objects, even when another file holds the same chunks under others. The
// file is read once, through one buffer. It returns the plaintext size it
// read and how many bytes of new ciphertext it wrote.
//
// On failure it removes the chunks it wrote, so a full disk is not left
// fuller for the next try.
func encryptChunks(rt *os.Root, r *repo.Repo, abs string, gear *gearTable, prev cacheEntry, known map[string]cachePart) (ce cacheEntry, size, newBytes int64, err error) {
	f, _, err := regular.Open(os.OpenFile, abs)
	if err != nil {
		return cacheEntry{}, 0, 0, err
	}
	defer f.Close()
	var wrote []string
	defer func() {
		if err != nil {
			for _, obj := range wrote {
				rt.Remove(filepath.FromSlash(obj))
			}
		}
	}()
	own := map[string]cachePart{}
	for _, p := range prev.all() {
		if p.Chunk != "" {
			own[p.Chunk] = p
		}
	}
	ch := newChunker(f, gear)
	whole := sha256.New()
	seen := map[string]cachePart{}
	var parts []cachePart
	for {
		b, err := ch.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cacheEntry{}, 0, 0, err
		}
		whole.Write(b)
		size += int64(len(b))
		s := sha256.Sum256(b)
		key := hex.EncodeToString(s[:])
		p, ok := seen[key]
		for _, m := range []map[string]cachePart{own, known} {
			if ok {
				break
			}
			if p, ok = m[key]; ok {
				p, ok = partIntact(rt, p)
			}
		}
		if !ok {
			if chunkHook != nil {
				if err := chunkHook(len(wrote)); err != nil {
					return cacheEntry{}, 0, 0, err
				}
			}
			obj, err := objectName(true, "")
			if err != nil {
				return cacheEntry{}, 0, 0, err
			}
			_, written, err := encryptTo(rt, obj, bytes.NewReader(b), r.Recipients, 0)
			if err != nil {
				return cacheEntry{}, 0, 0, err
			}
			wrote = append(wrote, obj)
			p = written[0]
			p.Chunk = key
			newBytes += p.CipherSize
		}
		seen[key] = p
		parts = append(parts, p)
	}
	return newCacheEntry(sum(whole), parts), size, newBytes, nil
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

// objectIntact reports whether every part of a cached file is still in the
// repo as seal wrote it (see partIntact), and returns the entry with the
// last-modified time of each part. An older salt wrote a large file as one
// object over GitHub's limit; that file is sealed again, in chunks, even
// though it has not changed.
func objectIntact(rt *os.Root, ce cacheEntry) (cacheEntry, bool) {
	parts := ce.all()
	out := make([]cachePart, len(parts))
	for i, p := range parts {
		var ok bool
		if out[i], ok = partIntact(rt, p); !ok {
			return ce, false
		}
	}
	return newCacheEntry(ce.SHA256, out), true
}

// partIntact reports whether one part is still in the repo as seal wrote
// it: a regular file (not a symlink) of the size it was written at, small
// enough to push, and last modified when it was written, if the cache
// recorded that (see cachePart). It returns the part with the file's
// last-modified time, so a cache from an older salt gains it.
func partIntact(rt *os.Root, p cachePart) (cachePart, bool) {
	if p.Object == "" || p.CipherSize > repo.GitHubFileLimit {
		return p, false
	}
	fi, err := rt.Lstat(filepath.FromSlash(p.Object))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != p.CipherSize {
		return p, false
	}
	mtime := fi.ModTime().UnixNano()
	if p.ModTime != 0 && p.ModTime != mtime {
		return p, false
	}
	p.ModTime = mtime
	return p, true
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
// unsafe or clashes with a file or folder already there. With foldCase,
// paths that differ only by case clash too, as they do with plain paths on
// macOS and Windows. Each path is looked up, not compared with every other,
// so the time grows with the number of files, not its square.
func addExtra(items []item, extra []Extra, foldCase bool) ([]item, error) {
	if len(extra) == 0 {
		return items, nil
	}
	t := newTaken(foldCase)
	for _, it := range items {
		t.add(it.rel)
	}
	for _, x := range extra {
		rel, err := repo.CleanPath(x.Rel)
		if err != nil {
			return nil, err
		}
		if _, other, ok := t.clash(rel); ok {
			if !Clash(other, rel) {
				return nil, fmt.Errorf("%w: %s and %s differ only by case, and with --plain-paths the repo would keep them as one file on macOS and Windows", ErrDuplicatePath, other, rel)
			}
			return nil, fmt.Errorf("%w: %s", ErrDuplicatePath, rel)
		}
		t.add(rel)
		items = append(items, item{rel: rel, abs: x.Path, mode: x.Mode, modTime: x.ModTime, live: x.Live, sha256: x.SHA256})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	return items, nil
}

// FirstClash returns the index i of the first cleaned slash path in rels
// that cannot be in a backup with one before it, and the index j of that
// one. With foldCase, paths that differ only by case clash too.
func FirstClash(rels []string, foldCase bool) (i, j int, ok bool) {
	t := newTaken(foldCase)
	for i, rel := range rels {
		if j, _, ok := t.clash(rel); ok {
			return i, j, true
		}
		t.add(rel)
	}
	return 0, 0, false
}

// taken holds the paths in a backup, and every folder above them, by key, so
// a clash is found by lookup. Each key maps to the index in paths of the
// path that took it.
type taken struct {
	key     func(string) string
	paths   []string
	files   map[string]int
	folders map[string]int
}

func newTaken(foldCase bool) *taken {
	key := func(s string) string { return s }
	if foldCase {
		key = foldKey
	}
	return &taken{key: key, files: map[string]int{}, folders: map[string]int{}}
}

// add records the cleaned slash path rel and the folders above it.
func (t *taken) add(rel string) {
	n := len(t.paths)
	t.paths = append(t.paths, rel)
	t.files[t.key(rel)] = n
	for i := range len(rel) {
		if rel[i] != '/' {
			continue
		}
		if k := t.key(rel[:i]); !hasKey(t.folders, k) {
			t.folders[k] = n
		}
	}
}

// clash returns the index of the path already taken that rel clashes with,
// and what in it rel clashes with: the path itself, the folder above it that
// rel would be a file at, or the file above rel.
func (t *taken) clash(rel string) (j int, other string, ok bool) {
	k := t.key(rel)
	if j, ok := t.files[k]; ok {
		return j, t.paths[j], true
	}
	if j, ok := t.folders[k]; ok {
		return j, firstParts(t.paths[j], strings.Count(rel, "/")+1), true
	}
	for i := range len(rel) {
		if rel[i] != '/' {
			continue
		}
		if j, ok := t.files[t.key(rel[:i])]; ok {
			return j, t.paths[j], true
		}
	}
	return 0, "", false
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// firstParts returns the first n parts of the slash path p.
func firstParts(p string, n int) string {
	parts := strings.SplitN(p, "/", n+1)
	return strings.Join(parts[:min(n, len(parts))], "/")
}

// foldKey returns s with each letter replaced by the smallest of the letters
// it equals ignoring case, so two strings are strings.EqualFold exactly when
// their keys are equal. "/" folds only to itself, so the key keeps a path's
// parts where they were.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			low = min(low, f)
		}
		b.WriteRune(low)
	}
	return b.String()
}

// Clash reports whether the cleaned slash paths a and b cannot both be in a
// backup, because they are the same path or one is a folder above the other.
// Paths are compared exactly, case included, as salt keeps every name as it
// was given: state.db and State.db are two names.
func Clash(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
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

// CheckDisjoint refuses a source inside the repo or a repo inside the source:
// either would seal ciphertext into itself or leak plaintext into the repo.
func CheckDisjoint(src, root string, fn func(string) string) error {
	a, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	b, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if Within(a, b) || Within(b, a) {
		return fmt.Errorf("source %s and repository %s must not contain each other", show(fn, a), show(fn, b))
	}
	return nil
}

// Within reports whether the path p is dir or inside it.
func Within(p, dir string) bool {
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
