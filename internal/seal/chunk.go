package seal

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
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
// maxChunk bytes at a time, in one buffer it reuses.
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

func newChunker(r io.Reader, gear *gearTable) *chunker {
	b := bits.Len(uint(avgChunk)) - 1
	// FastCDC's normalised chunking, at level 1: a harder test before
	// avgChunk and an easier one after it keep most chunks close to
	// avgChunk. Level 2, FastCDC's default, found the old boundaries again
	// more slowly after a change, adding up to 6 chunks for one change where
	// level 1 added up to 4. The masks test the top bits of the hash, which
	// depend on the last 64 bytes.
	return &chunker{
		r:     r,
		gear:  gear,
		buf:   make([]byte, maxChunk),
		maskS: ^uint64(0) << (64 - (b + 1)),
		maskL: ^uint64(0) << (64 - (b - 1)),
	}
}

// next returns the next chunk. It is valid only until next is called again.
// An empty stream is one empty chunk, so every file has at least one. After
// the last chunk next returns io.EOF.
func (c *chunker) next() ([]byte, error) {
	c.n = copy(c.buf, c.buf[c.cut:c.n])
	c.cut = 0
	for !c.eof && c.n < len(c.buf) {
		m, err := c.r.Read(c.buf[c.n:])
		c.n += m
		if errors.Is(err, io.EOF) {
			c.eof = true
		} else if err != nil {
			return nil, err
		}
	}
	if c.n == 0 && c.emitted {
		return nil, io.EOF
	}
	c.emitted = true
	c.cut = c.boundary(c.buf[:c.n])
	return c.buf[:c.cut], nil
}

// boundary returns the length of the chunk at the start of data, which
// holds maxChunk bytes unless the stream ends sooner.
func (c *chunker) boundary(data []byte) int {
	n := len(data)
	if n <= minChunk {
		return n
	}
	normal := min(avgChunk, n)
	var h uint64
	i := minChunk
	for ; i < normal; i++ {
		h = h<<1 + c.gear[data[i]]
		if h&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = h<<1 + c.gear[data[i]]
		if h&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
