package seal

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// smallChunks makes chunks of 4 KiB to 64 KiB, about 16 KiB on average, for
// one test, so files over 64 KiB are chunked.
func smallChunks(t *testing.T) {
	t.Helper()
	oldMin, oldAvg, oldMax := minChunk, avgChunk, maxChunk
	minChunk, avgChunk, maxChunk = 4<<10, 16<<10, 64<<10
	t.Cleanup(func() { minChunk, avgChunk, maxChunk = oldMin, oldAvg, oldMax })
}

// fixedKey gives the fixture a key that is the same on every run, so the
// chunk boundaries seal finds are too.
func (f *fixture) fixedKey() {
	f.t.Helper()
	id, err := keys.IdentityFromEntropy(bytes.Repeat([]byte{7}, 16))
	if err != nil {
		f.t.Fatal(err)
	}
	f.id, f.signer = id, keys.SigningKey(id)
	if err := repo.Write(f.root, f.repo.Format, []string{id.Recipient().String()}); err != nil {
		f.t.Fatal(err)
	}
	if f.repo, err = repo.Open(f.root); err != nil {
		f.t.Fatal(err)
	}
}

func testGear(seed byte) *gearTable {
	return chunkGear(bytes.Repeat([]byte{seed}, 32))
}

// chunksOf splits data with the gear table and returns the chunks.
func chunksOf(t *testing.T, r io.Reader, gear *gearTable) [][]byte {
	t.Helper()
	c := newChunker(r, gear)
	var out [][]byte
	for {
		b, err := c.next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, bytes.Clone(b))
	}
}

// hashes returns the SHA-256 of each chunk.
func hashes(chunks [][]byte) [][32]byte {
	out := make([][32]byte, len(chunks))
	for i, c := range chunks {
		out[i] = sha256.Sum256(c)
	}
	return out
}

// newChunks counts the chunks in b that are not in a.
func newChunks(a, b [][]byte) int {
	old := hashes(a)
	n := 0
	for _, h := range hashes(b) {
		if !slices.Contains(old, h) {
			n++
		}
	}
	return n
}

func TestChunkerSizesAndContent(t *testing.T) {
	smallChunks(t)
	data := noise(1 << 20)
	chunks := chunksOf(t, bytes.NewReader(data), testGear(1))
	if len(chunks) < 1<<20/maxChunk {
		t.Fatalf("1 MiB made %d chunks", len(chunks))
	}
	for i, c := range chunks {
		last := i == len(chunks)-1
		if len(c) > maxChunk || (!last && len(c) < minChunk) {
			t.Errorf("chunk %d is %d bytes", i, len(c))
		}
	}
	if got := bytes.Join(chunks, nil); !bytes.Equal(got, data) {
		t.Fatal("the chunks do not join back into the data")
	}
	// Most chunks end on the content, not at the maximum size.
	full := 0
	for _, c := range chunks {
		if len(c) == maxChunk {
			full++
		}
	}
	if full > len(chunks)/4 {
		t.Errorf("%d of %d chunks hit the maximum size", full, len(chunks))
	}
}

// The boundaries depend only on the content and the key, never on how the
// reader hands the data over.
func TestChunkerIsDeterministic(t *testing.T) {
	smallChunks(t)
	data := noise(512 << 10)
	a := chunksOf(t, bytes.NewReader(data), testGear(1))
	b := chunksOf(t, iotest.HalfReader(bytes.NewReader(data)), testGear(1))
	if !slices.EqualFunc(a, b, bytes.Equal) {
		t.Fatal("the same data split differently when read in small pieces")
	}
	if c := chunksOf(t, bytes.NewReader(data), testGear(2)); newChunks(a, c) < len(c)-1 {
		t.Fatalf("another key kept %d of %d chunks", len(c)-newChunks(a, c), len(c))
	}
}

// Bytes inserted or changed in the middle only change the chunks around
// them; the boundaries after them are found again.
func TestChunkerFindsBoundariesAgain(t *testing.T) {
	smallChunks(t)
	data := noise(1 << 20)
	before := chunksOf(t, bytes.NewReader(data), testGear(1))
	mid := len(data) / 2
	inserted := slices.Concat(data[:mid], []byte(strings.Repeat("new row ", 125)), data[mid:])
	changed := slices.Clone(data)
	copy(changed[mid:], noise(4096 + 1)[1:])
	for name, after := range map[string][]byte{"insert": inserted, "change": changed} {
		got := chunksOf(t, bytes.NewReader(after), testGear(1))
		if n := newChunks(before, got); n > 4 {
			t.Errorf("%s: %d of %d chunks are new", name, n, len(got))
		}
	}
}

func TestChunkerSmallAndEmpty(t *testing.T) {
	smallChunks(t)
	for _, n := range []int{0, 1, minChunk, minChunk + 1} {
		chunks := chunksOf(t, bytes.NewReader(noise(n)), testGear(1))
		if len(chunks) != 1 || len(chunks[0]) != n {
			t.Errorf("%d bytes made %d chunks", n, len(chunks))
		}
	}
}

func TestChunkerReadError(t *testing.T) {
	smallChunks(t)
	c := newChunker(io.MultiReader(bytes.NewReader(noise(1000)), errReader{}), testGear(1))
	if _, err := c.next(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("next = %v, want the read error", err)
	}
}

func TestChunkGearKeyed(t *testing.T) {
	if *testGear(1) != *testGear(1) {
		t.Fatal("the same seed gave two tables")
	}
	if *testGear(1) == *testGear(2) {
		t.Fatal("two seeds gave the same table")
	}
}

// newObjects lists the objects in after that are not in before.
func newObjects(before, after map[string]string) []string {
	var out []string
	for p := range after {
		if _, ok := before[p]; !ok && strings.HasPrefix(p, repo.ObjectsDir+"/") {
			out = append(out, p)
		}
	}
	return out
}

func TestChunkedRoundTrip(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			smallChunks(t)
			f := newFixture(t, encryptPaths)
			f.writeNoise("data/big.db", 1<<20)
			res := f.seal(false)
			if res.Files != 6 || res.Encrypted != 6 {
				t.Fatalf("result = %+v", res)
			}
			e := f.entry("data/big.db")
			if len(e.Parts) < 1<<20/maxChunk {
				t.Fatalf("1 MiB made %d chunks", len(e.Parts)+1)
			}
			for _, obj := range e.objects() {
				if !strings.HasPrefix(obj, repo.ObjectsDir+"/") {
					t.Errorf("chunk %s is not under objects/", obj)
				}
			}
			if v := f.indexVersion(); v != partsIndexVersion {
				t.Errorf("index version %d, want %d", v, partsIndexVersion)
			}
			i := slices.IndexFunc(res.Written, func(w Written) bool { return w.Path == "data/big.db" })
			if i < 0 || res.Written[i].Bytes < 1<<20 {
				t.Errorf("written = %+v", res.Written)
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

func TestChunkedUnchangedChangesNothing(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 1<<20)
	f.seal(false)
	before := snapshot(t, f.root)
	res := f.seal(false)
	if res.Encrypted != 0 || res.Reused != 6 || res.IndexNew || len(res.Removed) != 0 || len(res.Written) != 0 {
		t.Fatalf("second seal = %+v", res)
	}
	if after := snapshot(t, f.root); len(newObjects(before, after)) != 0 || len(after) != len(before) {
		t.Fatal("an unchanged chunked file changed the repo")
	}
}

// A change in the middle of a large file, in place or by inserting bytes,
// adds only the chunks around it and removes the ones it replaced.
func TestChunkedChangeAddsOnlyItsChunks(t *testing.T) {
	data := noise(1 << 20)
	mid := len(data) / 2
	changed := slices.Clone(data)
	copy(changed[mid:], noise(4097)[1:])
	cases := map[string][]byte{
		"in place": changed,
		"insert":   slices.Concat(data[:mid], []byte(strings.Repeat("new row ", 125)), data[mid:]),
		"page one": append(noise(4097)[1:], data[4096:]...),
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			smallChunks(t)
			f := newFixture(t, true)
			f.fixedKey()
			f.write("big.db", string(data))
			f.seal(false)
			chunks := len(f.entry("big.db").objects())
			before := snapshot(t, f.root)
			f.write("big.db", string(after))
			res := f.seal(false)
			added := newObjects(before, snapshot(t, f.root))
			if len(added) == 0 || len(added) > 4 || len(res.Removed) != len(added) {
				t.Fatalf("%d of %d chunks added, %d removed", len(added), chunks, len(res.Removed))
			}
			if res.Encrypted != 1 || len(res.Written) != 1 || res.Written[0].Bytes > int64(4*maxChunk+4096) {
				t.Fatalf("seal = %+v", res)
			}
			assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
		})
	}
}

// A chunk lost from the repo is the only one encrypted again.
func TestChunkedLostChunkIsEncryptedAgain(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 1<<20)
	f.seal(false)
	lost := f.entry("big.db").Parts[3]
	os.Remove(filepath.Join(f.root, filepath.FromSlash(lost)))
	before := snapshot(t, f.root)
	res := f.seal(false)
	if added := newObjects(before, snapshot(t, f.root)); len(added) != 1 || res.Encrypted != 1 {
		t.Fatalf("seal after losing a chunk = %+v, added %v", res, added)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// A chunk is reused wherever it appears: in a copy of a file, under a new
// name, and twice in one file. Each object is kept while any file uses it.
func TestChunksSharedBetweenFiles(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.fixedKey()
	data := noise(512 << 10)
	f.write("a.db", string(data))
	f.seal(false)
	before := snapshot(t, f.root)
	f.write("b.db", string(slices.Concat(data, data)))
	res := f.seal(false)
	if added := newObjects(before, snapshot(t, f.root)); len(added) > 2 {
		t.Fatalf("a copy of a chunked file added %d chunks: %+v", len(added), res)
	}
	os.Remove(filepath.Join(f.src, "a.db"))
	f.seal(false)
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
	vr, err := Verify(f.root, f.ids(), VerifyOptions{})
	if err != nil || vr.ProblemCount != 0 || len(vr.Unreferenced) != 0 {
		t.Fatalf("verify = %+v, %v", vr, err)
	}
}

// A seal that fails part way through a chunked file removes the chunks it
// had written, so a full disk is not left fuller for the next try. The seal
// fails as it is about to write the third new chunk, so two are written
// first.
func TestFailedChunkedSealLeavesNoChunks(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 256<<10)
	f.seal(false)
	f.write("big.db", string(noise(2 << 20)[1<<20:]))
	full := errors.New("no space left on device")
	most := 0
	chunkHook = func(written int) error {
		most = max(most, written)
		if written == 2 {
			return full
		}
		return nil
	}
	t.Cleanup(func() { chunkHook = nil })
	before := snapshot(t, f.root)
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer}); !errors.Is(err, full) || !strings.Contains(err.Error(), "big.db") {
		t.Fatalf("seal = %v", err)
	}
	if most != 2 {
		t.Fatalf("the seal failed after %d new chunks, want 2", most)
	}
	for p := range snapshot(t, f.root) {
		if _, ok := before[p]; !ok {
			t.Errorf("a failed seal left %s", p)
		}
	}
}

// failOn makes a one-worker seal fail as it is about to write the first new
// chunk of the file named name, after every file before it has finished.
func failOn(t *testing.T, name string) error {
	t.Helper()
	full := errors.New("no space left on device")
	failing := false
	hashedHook = func(p string) { failing = failing || filepath.Base(p) == name }
	chunkHook = func(int) error {
		if failing {
			return full
		}
		return nil
	}
	t.Cleanup(func() { hashedHook, chunkHook = nil, nil })
	return full
}

// A seal that fails keeps what the files before the failure wrote, so the
// next seal reuses a large file's chunks instead of encrypting it again.
func TestFailedSealKeepsFinishedFiles(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.writeNoise("a.db", 512<<10)
	f.write("b.db", string(noise(1 << 20)[512<<10:]))
	full := failOn(t, "b.db")
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer, Workers: 1}); !errors.Is(err, full) {
		t.Fatalf("seal = %v", err)
	}
	hashedHook, chunkHook = nil, nil
	res := f.seal(false)
	if slices.ContainsFunc(res.Written, func(w Written) bool { return w.Path == "a.db" }) {
		t.Fatalf("a.db, sealed before the failure, was encrypted again: %+v", res.Written)
	}
	if !slices.ContainsFunc(res.Written, func(w Written) bool { return w.Path == "b.db" }) {
		t.Fatalf("b.db was not sealed: %+v", res.Written)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
	vr, err := Verify(f.root, f.ids(), VerifyOptions{})
	if err != nil || vr.ProblemCount != 0 || len(vr.Unreferenced) != 0 {
		t.Fatalf("verify = %+v, %v", vr, err)
	}
}

// With plain paths a changed file's object is replaced in place. When the
// seal then fails on a later file and the file goes back to what it was,
// the object, which holds the newer content at the same size, is not kept
// for the older content: restore and verify still pass.
func TestFailedSealRecordsReplacedPlainObject(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, false)
	old := string(noise(8 << 10))
	f.write("x.bin", old)
	f.seal(false)
	f.write("x.bin", string(noise(16 << 10)[8<<10:]))
	f.writeNoise("y.db", 256<<10)
	full := failOn(t, "y.db")
	if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer, Workers: 1}); !errors.Is(err, full) {
		t.Fatalf("seal = %v", err)
	}
	hashedHook, chunkHook = nil, nil
	f.write("x.bin", old)
	f.seal(false)
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
	vr, err := Verify(f.root, f.ids(), VerifyOptions{})
	if err != nil || vr.ProblemCount != 0 {
		t.Fatalf("verify = %+v, %v", vr, err)
	}
}

// A new file whose chunks were all sealed before writes nothing new, but is
// counted as encrypted, not unchanged.
func TestNewFileFromKnownChunksIsNotUnchanged(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	data := string(noise(512 << 10))
	f.write("a.db", data)
	f.seal(false)
	f.write("b.db", data)
	if res := f.seal(false); res.Encrypted != 1 || res.Reused != 6 || len(res.Written) != 0 {
		t.Fatalf("seal of a copy = %+v", res)
	}
}

// Two files with the same chunks, both new in one seal, each keep their own
// objects, so the next seal of the unchanged snapshot changes nothing.
func TestSameNewChunksInTwoFilesStayPut(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	data := string(noise(512 << 10))
	f.write("a.db", data)
	f.write("b.db", data)
	f.seal(false)
	for range 3 {
		if res := f.seal(false); res.IndexNew || len(res.Removed) != 0 || res.Encrypted != 0 {
			t.Fatalf("an unchanged snapshot changed the repo: %+v", res)
		}
	}
}

// A large file in parts, as an older salt sealed it, keeps its parts while it
// is unchanged, and is chunked once it changes.
func TestOldPartsKeptUntilChanged(t *testing.T) {
	smallParts(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 256<<10)
	f.seal(false)
	if v := f.indexVersion(); v != partsIndexVersion {
		t.Fatalf("index version %d, want parts", v)
	}
	smallChunks(t)
	if res := f.seal(false); res.Encrypted != 0 || res.IndexNew {
		t.Fatalf("an unchanged file in parts was sealed again: %+v", res)
	}
	old := f.entry("big.db").objects()
	f.writeNoise("big.db", 300<<10)
	res := f.seal(false)
	if c, _ := f.loadCache(); !c.Files["big.db"].chunked() {
		t.Fatalf("a changed file in parts was not chunked: %+v", c.Files["big.db"])
	}
	slices.Sort(old)
	slices.Sort(res.Removed)
	if !slices.Equal(old, res.Removed) {
		t.Fatalf("removed %v, want the old parts %v", res.Removed, old)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// A chunked file that shrinks to maxChunk or less, even to nothing, is one
// object again, under its own name with plain paths, and is chunked again
// once it grows.
func TestChunkedFileShrinks(t *testing.T) {
	for _, encryptPaths := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted-paths", false: "plain-paths"}[encryptPaths], func(t *testing.T) {
			smallChunks(t)
			f := newFixture(t, encryptPaths)
			f.writeNoise("big.db", 256<<10)
			f.seal(false)
			chunked := func() bool {
				c, _ := f.loadCache()
				return c.Files["big.db"].chunked()
			}
			for _, content := range []string{"small now\n", "", string(noise(maxChunk))} {
				f.write("big.db", content)
				f.seal(false)
				e := f.entry("big.db")
				if len(e.Parts) != 0 || e.Size != int64(len(content)) || chunked() {
					t.Fatalf("%d bytes sealed as %+v", len(content), e)
				}
				if want := path.Join(repo.FilesDir, "big.db") + ".age"; !encryptPaths && e.Object != want {
					t.Errorf("%d bytes sealed as %s, want %s", len(content), e.Object, want)
				}
				assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
			}
			f.writeNoise("big.db", 256<<10)
			f.seal(false)
			if !chunked() {
				t.Fatal("a file that grew again was not chunked")
			}
		})
	}
}

// A live chunked file deleted before it is read is left out of the backup,
// as any live file is.
func TestLiveChunkedFileGone(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	x := Extra{Rel: "x.db", Path: filepath.Join(t.TempDir(), "x.db"), Live: true}
	if err := os.WriteFile(x.Path, noise(256<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sealExtra(x); err != nil {
		t.Fatal(err)
	}
	os.Remove(x.Path)
	res, err := f.sealExtra(x)
	if err != nil || !slices.Equal(res.Gone, []string{"x.db"}) {
		t.Fatalf("seal of a deleted live chunked file = %+v, %v", res, err)
	}
}

// A chunk that cannot be written, or a file that cannot be read, fails the
// seal with the file's path.
func TestChunkedFailures(t *testing.T) {
	smallChunks(t)
	t.Run("unwritable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can write to a read-only folder")
		}
		f := newFixture(t, true)
		f.writeNoise("big.db", 256<<10)
		f.seal(false)
		f.writeNoise("big.db", 300<<10)
		// Every folder under objects/ is made read-only, so only the changed
		// file, which needs new chunks, fails.
		dirs, _ := filepath.Glob(filepath.Join(f.root, repo.ObjectsDir, "*"))
		dirs = append(dirs, filepath.Join(f.root, repo.ObjectsDir))
		for _, d := range dirs {
			os.Chmod(d, 0o500)
		}
		t.Cleanup(func() {
			for _, d := range dirs {
				os.Chmod(d, 0o700)
			}
		})
		if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer}); err == nil || !strings.Contains(err.Error(), "big.db") {
			t.Fatalf("seal = %v", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can read any file")
		}
		f := newFixture(t, true)
		f.writeNoise("big.db", 256<<10)
		f.seal(false)
		p := filepath.Join(f.src, "big.db")
		os.Chmod(p, 0)
		t.Cleanup(func() { os.Chmod(p, 0o600) })
		if _, err := Seal(f.src, f.repo, Options{CacheDir: f.cache, Signer: f.signer}); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "big.db") {
			t.Fatalf("seal of an unreadable chunked file = %v", err)
		}
	})
	t.Run("removed", func(t *testing.T) {
		f := newFixture(t, true)
		x := Extra{Rel: "x.db", Path: filepath.Join(t.TempDir(), "x.db"), Mode: 0o600}
		if err := os.WriteFile(x.Path, noise(256<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.sealExtra(x); err != nil {
			t.Fatal(err)
		}
		os.Remove(x.Path)
		if _, err := f.sealExtra(x); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "x.db") {
			t.Fatalf("seal of a removed chunked file = %v", err)
		}
	})
}

// Sealing in chunks allocates little more than sealing the same file as one
// object: the buffer is reused, and so are the zstd encoders. A garbage
// collection can empty the pool of encoders, so making a few new ones, about
// 5 MiB each, is allowed for; making one per chunk is not.
func TestChunkedStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("writes two 8 MiB files")
	}
	const size, perChunk = 8 << 20, 128 << 10
	alloc := func(fn func()) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		fn()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	run := func() (uint64, int) {
		f := newFixture(t, true)
		f.writeNoise("big.db", size)
		n := alloc(func() { f.seal(false) })
		assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
		return n, len(f.entry("big.db").objects())
	}
	whole, n := run()
	if n != 1 {
		t.Fatalf("the whole file made %d objects", n)
	}
	oldMin, oldAvg, oldMax := minChunk, avgChunk, maxChunk
	minChunk, avgChunk, maxChunk = 64<<10, 256<<10, 1<<20
	t.Cleanup(func() { minChunk, avgChunk, maxChunk = oldMin, oldAvg, oldMax })
	chunked, chunks := run()
	if chunks < size/maxChunk {
		t.Fatalf("8 MiB made %d chunks", chunks)
	}
	if budget := uint64(chunks*perChunk + 16<<20); chunked > whole+budget {
		t.Errorf("seal in %d chunks allocated %d KiB more than in one", chunks, (chunked-whole)>>10)
	}
}
