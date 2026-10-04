package seal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// smallParts makes files over 64 KiB split into 16 KiB parts, and caps a
// file that grows past 64 KiB while it is sealed at 80 KiB, for one test.
func smallParts(t *testing.T) {
	t.Helper()
	oldSplit, oldPart, oldOne := splitAbove, partSize, oneObjectLimit
	splitAbove, partSize, oneObjectLimit = 64<<10, 16<<10, 80<<10
	t.Cleanup(func() { splitAbove, partSize, oneObjectLimit = oldSplit, oldPart, oldOne })
}

// noise returns n bytes that do not compress, the same on every run.
func noise(n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{1}).Read(b)
	return b
}

// writeNoise writes n bytes of noise to rel in the source tree.
func (f *fixture) writeNoise(rel string, n int) {
	f.t.Helper()
	f.write(rel, string(noise(n)))
}

// entry returns the index entry for rel.
func (f *fixture) entry(rel string) Entry {
	f.t.Helper()
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range ix.Entries {
		if e.Path == rel {
			return e
		}
	}
	f.t.Fatalf("%s is not in the index", rel)
	return Entry{}
}

// indexVersion returns the version of the repo's index.
func (f *fixture) indexVersion() int {
	f.t.Helper()
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	return ix.Version
}

// maxCipherSize is the most a part of partSize compressed bytes can take
// once encrypted: age adds 16 bytes per 64 KiB chunk and a short header.
func maxCipherSize() int64 {
	return partSize + (partSize/(64<<10)+1)*16 + 1024
}

func TestSplitRoundTrip(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			smallParts(t)
			f := newFixture(t, encryptPaths)
			f.writeNoise("data/big.db", 200<<10)
			res := f.seal(false)
			if res.Files != 6 || res.Encrypted != 6 {
				t.Fatalf("result = %+v", res)
			}
			e := f.entry("data/big.db")
			if len(e.Parts) < 12 {
				t.Fatalf("200 KiB in 16 KiB parts made %d extra parts", len(e.Parts))
			}
			if want := path.Join(repo.FilesDir, "data/big.db") + ".age"; !encryptPaths && e.Object != want {
				t.Errorf("first part is %s, want %s", e.Object, want)
			}
			for _, obj := range e.objects() {
				fi, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(obj)))
				if err != nil {
					t.Fatal(err)
				}
				if fi.Size() > maxCipherSize() {
					t.Errorf("part %s is %d bytes, over the %d limit", obj, fi.Size(), maxCipherSize())
				}
			}
			for _, obj := range e.Parts {
				if !strings.HasPrefix(obj, repo.ObjectsDir+"/") || !strings.HasSuffix(obj, ".age") {
					t.Errorf("later part %s is not a random name under objects/", obj)
				}
			}
			if v := f.indexVersion(); v != partsIndexVersion {
				t.Errorf("index version %d, want %d", v, partsIndexVersion)
			}
			assertAllCiphertext(t, f.root, encryptPaths)
			assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
			vr, err := Verify(f.root, f.ids(), VerifyOptions{})
			if err != nil || vr.ProblemCount != 0 || len(vr.Unreferenced) != 0 || vr.Files != 6 {
				t.Fatalf("verify = %+v, %v", vr, err)
			}
		})
	}
}

// A file up to splitAbove is one object, even when it does not compress and
// is far bigger than a part; so is a larger file that compresses into one
// part. Neither changes the index version.
func TestNoSplitAtOrBelowTheLimit(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("at-limit.bin", int(splitAbove))
	f.write("compresses.txt", strings.Repeat("the same line\n", 20000))
	f.seal(false)
	for _, rel := range []string{"at-limit.bin", "compresses.txt"} {
		if e := f.entry(rel); len(e.Parts) != 0 {
			t.Errorf("%s was split into %d parts", rel, len(e.Parts)+1)
		}
	}
	if v := f.indexVersion(); v != repo.FormatVersion {
		t.Errorf("index version %d, want %d", v, repo.FormatVersion)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// Compression and age add less than 0.1% to data that does not compress,
// the margin TestOneObjectLimitFits allows for.
func TestOneObjectGrowth(t *testing.T) {
	const n = 4 << 20
	rt := openRoot(t, t.TempDir())
	f := newFixture(t, true)
	_, parts, err := encryptTo(rt, "obj.age", bytes.NewReader(noise(n)), f.repo.Recipients, 0)
	if err != nil || len(parts) != 1 {
		t.Fatalf("encryptTo = %v, %v", parts, err)
	}
	if growth := float64(parts[0].CipherSize-n) / n; growth > 0.001 {
		t.Fatalf("ciphertext is %.3f%% bigger than the file", growth*100)
	}
}

// A file measured at or under splitAbove that grows before it is encrypted,
// as a live file can, still never makes an object over oneObjectLimit: the
// rest goes into a second part. The backup holds the grown file.
func TestFileThatGrowsWhileSealed(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("live.db", 60<<10)
	grown := noise(160 << 10)
	hashedHook = func(p string) {
		if filepath.Base(p) == "live.db" {
			os.WriteFile(p, grown, 0o644)
		}
	}
	t.Cleanup(func() { hashedHook = nil })
	f.seal(false)
	e := f.entry("live.db")
	if len(e.Parts) == 0 {
		t.Fatal("a file that grew past the cap is one object")
	}
	for _, obj := range e.objects() {
		fi, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(obj)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > oneObjectLimit+1024 {
			t.Errorf("part %s is %d bytes, over the %d cap", obj, fi.Size(), oneObjectLimit)
		}
	}
	got, err := os.ReadFile(filepath.Join(f.restore(RestoreOptions{}), "live.db"))
	if err != nil || !bytes.Equal(got, grown) {
		t.Fatalf("restored live.db is %d bytes, want the grown %d: %v", len(got), len(grown), err)
	}
}

// The cap leaves room for what age adds to a full object, and is above
// anything a file of splitAbove bytes compresses to, so such a file is
// never cut in two.
func TestOneObjectLimitFits(t *testing.T) {
	ageCost := (oneObjectLimit/(64<<10) + 1) * 16
	if oneObjectLimit+ageCost+4096 >= repo.GitHubFileLimit {
		t.Fatalf("an object of %d compressed bytes can reach GitHub's limit", oneObjectLimit)
	}
	if float64(splitAbove)*1.001 >= float64(oneObjectLimit) {
		t.Fatalf("a %d byte file that does not compress could be cut at %d", splitAbove, oneObjectLimit)
	}
}

func TestSplitUnchangedChangesNothing(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 100<<10)
	f.seal(false)
	before := snapshot(t, f.root)
	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 6 || res.IndexNew || len(res.Removed) != 0 {
		t.Fatalf("second seal = %+v", res)
	}
	if after := snapshot(t, f.root); !maps.Equal(before, after) {
		t.Fatal("an unchanged split file changed the repo")
	}
}

// A file that changes or shrinks back to one object leaves no old part
// behind.
func TestSplitChangeRemovesOldParts(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 100<<10)
	f.seal(false)
	old := f.entry("big.db").objects()
	f.write("big.db", "small now\n")
	res := f.seal(false)
	slices.Sort(old)
	slices.Sort(res.Removed)
	if !slices.Equal(res.Removed, old) {
		t.Fatalf("removed %v, want the old parts %v", res.Removed, old)
	}
	if v := f.indexVersion(); v != repo.FormatVersion {
		t.Errorf("index version %d after the split file shrank", v)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// A part lost from the repo makes seal encrypt the file again.
func TestSplitLostPartIsEncryptedAgain(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 100<<10)
	f.seal(false)
	parts := f.entry("big.db").Parts
	os.Remove(filepath.Join(f.root, filepath.FromSlash(parts[len(parts)-1])))
	if res := f.seal(false); res.Encrypted != 1 {
		t.Fatalf("seal after losing a part = %+v", res)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// Every way a part can go wrong is reported by verify and stops restore.
func TestSplitDamagedParts(t *testing.T) {
	cases := map[string]struct {
		damage func(f *fixture, e Entry)
		want   string
	}{
		"missing": {
			damage: func(f *fixture, e Entry) {
				os.Remove(filepath.Join(f.root, filepath.FromSlash(e.Parts[1])))
			},
			want: "is missing",
		},
		"truncated": {
			damage: func(f *fixture, e Entry) {
				p := filepath.Join(f.root, filepath.FromSlash(e.Parts[1]))
				fi, _ := os.Stat(p)
				os.Truncate(p, fi.Size()-100)
			},
			want: "cannot be decrypted",
		},
		"another key": {
			damage: func(f *fixture, e Entry) {
				other, _ := age.GenerateX25519Identity()
				rt := openRoot(f.t, f.root)
				os.Remove(filepath.Join(f.root, filepath.FromSlash(e.Parts[1])))
				if _, _, err := encryptTo(rt, e.Parts[1], strings.NewReader("x"), []age.Recipient{other.Recipient()}, 0); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "none of your keys",
		},
		"not age": {
			damage: func(f *fixture, e Entry) {
				os.WriteFile(filepath.Join(f.root, filepath.FromSlash(e.Parts[1])), []byte("not an age file\n"), 0o644)
			},
			want: "cannot be decrypted",
		},
		"out of order": {
			damage: func(f *fixture, _ Entry) {
				f.rewriteIndex(func(e *Entry) { slices.Reverse(e.Parts) })
			},
			want: "cannot be decrypted",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			smallParts(t)
			f := newFixture(t, true)
			f.writeNoise("big.db", 100<<10)
			f.seal(false)
			c.damage(f, f.entry("big.db"))
			vr, err := Verify(f.root, f.ids(), VerifyOptions{})
			if err != nil || vr.ProblemCount != 1 || !strings.Contains(vr.Problems[0], c.want) {
				t.Fatalf("verify = %+v, %v; want a problem with %q", vr, err, c.want)
			}
			dest := filepath.Join(t.TempDir(), "r")
			if _, err := Restore(f.root, f.ids(), dest, RestoreOptions{}); err == nil {
				t.Fatal("restore of a damaged split file succeeded")
			}
			if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a failed restore left %s: %v", dest, err)
			}
		})
	}
}

// A stream that fills its parts exactly makes no empty last part, and the
// parts read back as the stream that was written.
func TestPartWriterAndReader(t *testing.T) {
	f := newFixture(t, true)
	dir := t.TempDir()
	rt := openRoot(t, dir)
	for _, size := range []int{0, 1, 4096, 4097, 3 * 4096} {
		w := &partWriter{rt: rt, recipients: f.repo.Recipients, limit: 4096}
		if err := w.open("first.age"); err != nil {
			t.Fatal(err)
		}
		data := noise(size)
		// Odd-sized writes cross the part boundaries.
		for b := data; len(b) > 0; {
			n := min(len(b), 1000)
			if _, err := w.Write(b[:n]); err != nil {
				t.Fatal(err)
			}
			b = b[n:]
		}
		if err := w.closePart(); err != nil {
			t.Fatal(err)
		}
		if err := w.commit(); err != nil {
			t.Fatal(err)
		}
		if want := max(1, (size+4095)/4096); len(w.parts) != want {
			t.Fatalf("%d bytes made %d parts, want %d", size, len(w.parts), want)
		}
		var names []string
		for _, p := range w.parts {
			names = append(names, p.Object)
		}
		pr := &partReader{rt: rt, ids: f.ids(), names: names}
		got, err := io.ReadAll(pr)
		pr.close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%d bytes read back as %d, %v", size, len(got), err)
		}
	}
}

// A failure part way through a split file leaves no part and no temporary
// file behind: a failing source, a later part whose folder can't be made,
// and a first part that can't be moved into place after the others were.
func TestEncryptToSplitFailureLeavesNothing(t *testing.T) {
	f := newFixture(t, true)
	cases := map[string]struct {
		prepare func(dir string)
		r       io.Reader
	}{
		"failing source": {func(string) {}, io.MultiReader(bytes.NewReader(noise(64<<10)), errReader{})},
		"no objects folder": {func(dir string) {
			os.WriteFile(filepath.Join(dir, repo.ObjectsDir), []byte("x"), 0o644)
		}, bytes.NewReader(noise(64 << 10))},
		"first part blocked": {func(dir string) {
			os.MkdirAll(filepath.Join(dir, "obj.age", "full"), 0o755)
		}, bytes.NewReader(noise(64 << 10))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c.prepare(dir)
			before := snapshot(t, dir)
			rt := openRoot(t, dir)
			if _, _, err := encryptTo(rt, "obj.age", c.r, f.repo.Recipients, 4096); err == nil {
				t.Fatal("encryptTo succeeded")
			}
			// Parts already renamed into place are left for the next seal to
			// remove, as the index never names them; nothing else may remain.
			for p := range snapshot(t, dir) {
				if _, ok := before[p]; !ok && (strings.Contains(p, ".salt-tmp-") || !strings.HasPrefix(p, repo.ObjectsDir+"/")) {
					t.Errorf("a failed encryptTo left %s", p)
				}
			}
		})
	}
}

// Splitting adds only a small, fixed cost per part: sealing and restoring a
// file in parts allocates little more than the same file in one object.
// (zstd's own buffers are a cost per block, so both totals grow with a file
// that does not compress; peak memory stays flat.)
func TestSplitStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("writes two 16 MiB files")
	}
	const size, perPart = 16 << 20, 512 << 10
	smallParts(t)
	alloc := func(fn func()) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		fn()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	run := func(part int64) (seal, restore uint64, parts int) {
		partSize = part
		f := newFixture(t, true)
		f.writeNoise("big.db", size)
		seal = alloc(func() { f.seal(false) })
		var dest string
		restore = alloc(func() { dest = f.restore(RestoreOptions{}) })
		assertTreesEqual(t, f.src, dest)
		return seal, restore, len(f.entry("big.db").objects())
	}
	wholeSeal, wholeRestore, n := run(size * 2)
	if n != 1 {
		t.Fatalf("the whole file made %d parts", n)
	}
	splitSeal, splitRestore, parts := run(1 << 20)
	if parts < 16 {
		t.Fatalf("16 MiB in 1 MiB parts made %d parts", parts)
	}
	budget := uint64(parts * perPart)
	if splitSeal > wholeSeal+budget {
		t.Errorf("seal in %d parts allocated %d KiB more than in one", parts, (splitSeal-wholeSeal)>>10)
	}
	if splitRestore > wholeRestore+budget {
		t.Errorf("restore of %d parts allocated %d KiB more than of one", parts, (splitRestore-wholeRestore)>>10)
	}
}

// A cache written before files were split has the object and its size at
// the top level, and still reads as the first part.
func TestCacheFromBeforeParts(t *testing.T) {
	var ce cacheEntry
	if err := json.Unmarshal([]byte(`{"sha256":"ab","object":"objects/aa/b.age","cipher_size":12}`), &ce); err != nil {
		t.Fatal(err)
	}
	if ce.Object != "objects/aa/b.age" || ce.CipherSize != 12 || len(ce.Parts) != 0 {
		t.Fatalf("old cache entry read as %+v", ce)
	}
	b, _ := json.Marshal(ce)
	if string(b) != `{"sha256":"ab","object":"objects/aa/b.age","cipher_size":12}` {
		t.Fatalf("one-part cache entry written as %s", b)
	}
}

func TestReadIndexRejectsBadParts(t *testing.T) {
	sha := strings.Repeat("0", 64)
	cases := map[string]Entry{
		"unsafe part":       {Path: "a", Object: "objects/aa/b.age", Parts: []string{"../../etc/passwd"}, SHA256: sha},
		"empty part":        {Path: "a", Object: "objects/aa/b.age", Parts: []string{""}, SHA256: sha},
		"long part":         {Path: "a", Object: "objects/aa/b.age", Parts: []string{strings.Repeat("a", maxIndexString+1)}, SHA256: sha},
		"symlink with part": {Path: "a", Symlink: "b", Parts: []string{"objects/aa/b.age"}},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			f.writeIndex(&Index{Version: partsIndexVersion, Entries: []Entry{e}})
			if _, err := ReadIndex(f.root, f.ids(), false); err == nil {
				t.Fatal("ReadIndex accepted it")
			}
		})
	}
}

// loadCache returns the fixture's change cache and where it is kept.
func (f *fixture) loadCache() (*cache, string) {
	f.t.Helper()
	p, err := cachePath(f.cache, f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	c := loadCache(p, cacheKey(f.repo.RecipientStrings, f.repo.Format.EncryptPaths))
	if len(c.Files) == 0 {
		f.t.Fatal("the cache is empty")
	}
	return c, p
}

// An object over GitHub's limit, as an older salt wrote for a large file, is
// not reused even though the file has not changed: seal encrypts the file
// again, so the repo can be pushed. An object at the limit is still reused.
func TestOversizedObjectIsSealedAgain(t *testing.T) {
	for _, size := range []int64{repo.GitHubFileLimit, repo.GitHubFileLimit + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			f := newFixture(t, true)
			f.seal(false)
			c, cPath := f.loadCache()
			const rel = "data/memory.db"
			ce := c.Files[rel]
			// Truncate makes the object sparse, so it takes no space on disk.
			if err := os.Truncate(filepath.Join(f.root, filepath.FromSlash(ce.Object)), size); err != nil {
				t.Fatal(err)
			}
			ce.CipherSize = size
			c.Files[rel] = ce
			if err := c.save(cPath); err != nil {
				t.Fatal(err)
			}
			res := f.seal(false)
			if size <= repo.GitHubFileLimit {
				if res.Encrypted != 0 {
					t.Fatalf("an object at the limit was encrypted again: %+v", res)
				}
				return
			}
			if res.Encrypted != 1 || !slices.Equal(res.Removed, []string{ce.Object}) {
				t.Fatalf("seal = %+v, want %s encrypted again and removed", res, ce.Object)
			}
			assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
		})
	}
}

// A split file's cache entry has an empty top-level object, so an older
// salt, which reads only that one object, finds nothing to reuse and
// encrypts the file again rather than keeping its first part alone.
func TestSplitCacheEntryHiddenFromOlderSalt(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 100<<10)
	f.seal(false)
	_, cPath := f.loadCache()
	b, err := os.ReadFile(cPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Files map[string]json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var older struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(raw.Files["big.db"], &older); err != nil {
		t.Fatal(err)
	}
	if older.Object != "" {
		t.Fatalf("an older salt would reuse %s alone: %s", older.Object, raw.Files["big.db"])
	}
	var ce cacheEntry
	if err := json.Unmarshal(raw.Files["big.db"], &ce); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ce.all() {
		got = append(got, p.Object)
	}
	if want := f.entry("big.db").objects(); !slices.Equal(got, want) {
		t.Fatalf("cached parts %v, want %v", got, want)
	}
}

// newCacheEntry keeps one part at the top level and several in Parts, and
// all returns them in order either way.
func TestCacheEntryParts(t *testing.T) {
	one := []cachePart{{Object: "objects/aa/a.age", CipherSize: 10}}
	two := append(slices.Clone(one), cachePart{Object: "objects/bb/b.age", CipherSize: 5})
	for _, parts := range [][]cachePart{one, two} {
		ce := newCacheEntry("ab", parts)
		if !slices.Equal(ce.all(), parts) {
			t.Errorf("all() = %v, want %v", ce.all(), parts)
		}
		if split := len(parts) > 1; split != (ce.Object == "") || split != (len(ce.Parts) > 0) {
			t.Errorf("%d parts stored as %+v", len(parts), ce)
		}
	}
}

// An index with a part name that is not valid UTF-8 still verifies: the
// signature covers the name as ReadIndex decodes it.
func TestSignatureCoversInvalidUTF8Part(t *testing.T) {
	f := newFixture(t, true)
	f.writeIndex(&Index{Version: partsIndexVersion, Entries: []Entry{
		{Path: "a", Object: "objects/aa/a.age", Parts: []string{"objects/bb/\xff.age"}, SHA256: strings.Repeat("0", 64), Mode: 0o644},
	}})
	ix, err := ReadIndex(f.root, f.ids(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ix.Entries[0].Parts[0]; got != "objects/bb/�.age" {
		t.Fatalf("part read back as %q", got)
	}
}

// A part that cannot be written, as on a full disk, fails the write and
// leaves no temporary file once aborted: a failed write of a full chunk,
// and a failed close when a part reaches its limit or the stream ends.
func TestPartWriterWriteFailures(t *testing.T) {
	f := newFixture(t, true)
	cases := map[string]func(w *partWriter) error{
		"write": func(w *partWriter) error {
			_, err := w.Write(noise(128 << 10))
			return err
		},
		"close at the limit": func(w *partWriter) error {
			w.limit = 1024
			_, err := w.Write(noise(1024))
			return err
		},
		"close at the end": func(w *partWriter) error {
			return w.closePart()
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			w := &partWriter{rt: openRoot(t, dir), recipients: f.repo.Recipients}
			if err := w.open("obj.age"); err != nil {
				t.Fatal(err)
			}
			w.f.Close() // every later write to the part fails
			if err := fail(w); err == nil {
				t.Fatal("writing to a closed part succeeded")
			}
			w.abort()
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("an aborted write left %v", entries)
			}
		})
	}
}

// A part that cannot be created fails the write and leaves nothing behind.
func TestEncryptToUnwritableFolder(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to a read-only folder")
	}
	f := newFixture(t, true)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	if _, _, err := encryptTo(openRoot(t, dir), "ro/obj.age", strings.NewReader("x"), f.repo.Recipients, 0); err == nil {
		t.Fatal("encryptTo into a read-only folder succeeded")
	}
	if entries, _ := os.ReadDir(ro); len(entries) != 0 {
		t.Fatalf("a failed encryptTo left %v", entries)
	}
}

// A part whose last read returns data together with io.EOF still yields
// that data, and the stream ends after it rather than reading the finished
// part again forever.
func TestPartReaderDataWithEOF(t *testing.T) {
	pr := &partReader{r: iotest.DataErrReader(strings.NewReader("last bytes"))}
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(pr)
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || string(r.b) != "last bytes" {
			t.Fatalf("read %q, %v", r.b, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a finished part never ended")
	}
}

// A file removed after seal measured it, as a live file can be, fails the
// seal with its path, and nothing half-written is left in the repo.
func TestSealFileRemovedWhileSealed(t *testing.T) {
	f := newFixture(t, true)
	hashedHook = func(p string) {
		if filepath.Base(p) == "SOUL.md" {
			os.Remove(p)
		}
	}
	t.Cleanup(func() { hashedHook = nil })
	_, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer})
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "SOUL.md") {
		t.Fatalf("seal of a removed file: %v", err)
	}
	for p := range snapshot(t, f.root) {
		if strings.Contains(p, ".salt-tmp-") {
			t.Errorf("a failed seal left %s", p)
		}
	}
}

// An index that cannot be written fails the seal.
func TestSealIndexWriteFails(t *testing.T) {
	f := newFixture(t, true)
	os.MkdirAll(filepath.Join(f.root, repo.IndexFile, "full"), 0o755)
	_, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer})
	if err == nil || !strings.Contains(err.Error(), "writing index") {
		t.Fatalf("seal with an index that cannot be written: %v", err)
	}
}
