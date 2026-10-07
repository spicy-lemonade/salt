package seal

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
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
			if v := f.indexVersion(); v != chunksIndexVersion {
				t.Errorf("index version %d, want %d", v, chunksIndexVersion)
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
	if v := f.indexVersion(); v != chunksIndexVersion {
		t.Fatalf("index version %d, want chunks", v)
	}
	slices.Sort(old)
	slices.Sort(res.Removed)
	if !slices.Equal(old, res.Removed) {
		t.Fatalf("removed %v, want the old parts %v", res.Removed, old)
	}
	assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
}

// A chunked file that shrinks, even to nothing, is still sealed in chunks
// and restores as it is now.
func TestChunkedFileShrinks(t *testing.T) {
	smallChunks(t)
	f := newFixture(t, true)
	f.writeNoise("big.db", 256<<10)
	f.seal(false)
	for _, content := range []string{"small now\n", ""} {
		f.write("big.db", content)
		f.seal(false)
		if e := f.entry("big.db"); len(e.Parts) != 0 || e.Size != int64(len(content)) {
			t.Fatalf("%d bytes sealed as %+v", len(content), e)
		}
		assertTreesEqual(t, f.src, f.restore(RestoreOptions{}))
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
