package seal

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"sync"
)

// A file over maxChunk bytes is sealed in chunks of plaintext, each
// compressed and encrypted as its own object, so a change to one part of a
// large file adds only the chunks around it (see encryptChunks). Chunk
// boundaries follow the content (FastCDC), so bytes inserted or removed move
// only the boundaries near them. A chunk is at least minChunk bytes, usually
// near avgChunk, and at most maxChunk, apart from the last.
//
// avgChunk must be a power of two. They are variables so tests can use
// small chunks.
var (
	minChunk = 1 << 20
	avgChunk = 4 << 20
	maxChunk = 16 << 20
)

// Changing gearSalt or gearInfo moves every chunk boundary, so the next seal
// of each chunked file encrypts all of it again. Nothing already sealed
// stops restoring, since restore never needs the boundaries.
const (
	gearSalt = "salt chunk boundaries v1"
	gearInfo = "gear table"
)

// gearTable is the table of random numbers FastCDC's rolling hash adds up.
type gearTable [256]uint64

// chunkGear derives the gear table from the signing key's seed. With a
// table anyone can work out, the sizes of a file's chunks, which a reader of
// the repo can roughly see, would help tell which known file it is. The
// signing key is on every machine that seals the repo and never in it.
func chunkGear(seed []byte) *gearTable {
	// hkdf.Key fails only for keys longer than 255 SHA-256 blocks, and the
	// table is 64 blocks.
	b, _ := hkdf.Key(sha256.New, seed, []byte(gearSalt), gearInfo, 256*8)
	var g gearTable
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return &g
}

// chunker splits a stream into content-defined chunks. It holds at most
// maxChunk bytes at a time, in one buffer it reuses, taken from chunkBufs.
// It reads a step at a time (see readStep) while it looks for a boundary,
// so it has read little of the next chunk when one ends, and moves only
// that little to the start of the buffer for the next.
type chunker struct {
	r            io.Reader
	gear         *gearTable
	buf          []byte
	n            int // bytes held in buf
	cut          int // length of the chunk last returned, still at the start of buf
	eof          bool
	emitted      bool
	maskS, maskL uint64
}

// chunkBufs holds the buffers of chunkers that are done, so each large file
// does not need a new one of maxChunk bytes. At most one per worker is in
// use at a time.
var chunkBufs sync.Pool

// readStep is how much a chunker reads at a time.
func readStep() int { return max(avgChunk/8, 1) }

func newChunker(r io.Reader, gear *gearTable) *chunker {
	b := bits.Len(uint(avgChunk)) - 1
	buf, ok := chunkBufs.Get().(*[]byte)
	if !ok || len(*buf) != maxChunk {
		fresh := make([]byte, maxChunk)
		buf = &fresh
	}
	// FastCDC's normalised chunking, at level 1: a harder test before
	// avgChunk and an easier one after it keep most chunks close to
	// avgChunk. Level 2, FastCDC's default, found the old boundaries again
	// more slowly after a change, adding up to 6 chunks for one change where
	// level 1 added up to 4. The masks test the top bits of the hash, which
	// depend on the last 64 bytes.
	return &chunker{
		r:     r,
		gear:  gear,
		buf:   *buf,
		maskS: ^uint64(0) << (64 - (b + 1)),
		maskL: ^uint64(0) << (64 - (b - 1)),
	}
}

// release gives the chunker's buffer back for another to use. Neither the
// chunker nor the last chunk it returned may be used after.
func (c *chunker) release() {
	buf := c.buf
	c.buf = nil
	chunkBufs.Put(&buf)
}

// next returns the next chunk. It is valid only until next is called again.
// An empty stream is one empty chunk, so every file has at least one. After
// the last chunk next returns io.EOF.
//
// A chunk ends after the first byte from minChunk on where the rolling hash
// passes the test for where it is (see newChunker), at maxChunk bytes, or at
// the end of the stream. The hash starts afresh at minChunk, so a boundary
// depends only on the bytes since then, never on how they were read.
func (c *chunker) next() ([]byte, error) {
	c.n = copy(c.buf, c.buf[c.cut:c.n])
	c.cut = 0
	var h uint64
	i := minChunk
	gear, maskS, maskL := c.gear, c.maskS, c.maskL
	for {
		// The bytes read so far are scanned through locals, which keep the
		// loop as fast as one over the whole buffer.
		data := c.buf[:c.n]
		for end := min(len(data), avgChunk); i < end; i++ {
			h = h<<1 + gear[data[i]]
			if h&maskS == 0 {
				return c.chunk(i + 1), nil
			}
		}
		for ; i < len(data); i++ {
			h = h<<1 + gear[data[i]]
			if h&maskL == 0 {
				return c.chunk(i + 1), nil
			}
		}
		if c.eof || c.n == len(c.buf) {
			break
		}
		if err := c.fill(); err != nil {
			return nil, err
		}
	}
	if c.n == 0 && c.emitted {
		return nil, io.EOF
	}
	return c.chunk(c.n), nil
}

// chunk returns the first n bytes held as the next chunk.
func (c *chunker) chunk(n int) []byte {
	c.emitted, c.cut = true, n
	return c.buf[:n]
}

// fill reads up to readStep more bytes, stopping early at the end of the
// stream or of the buffer.
func (c *chunker) fill() error {
	end := min(len(c.buf), c.n+readStep())
	for !c.eof && c.n < end {
		m, err := c.r.Read(c.buf[c.n:end])
		c.n += m
		if errors.Is(err, io.EOF) {
			c.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}
