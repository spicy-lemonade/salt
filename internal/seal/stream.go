// Package seal encrypts a directory tree into a salt repository and restores
// it. All data is streamed: plaintext → zstd → age → file, with bounded
// buffers, so memory use does not grow with file size.
package seal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sync"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"github.com/spicy-lemonade/salt/internal/regular"
)

// zstd settings chosen for bounded memory rather than maximum ratio.
const (
	zstdWindow    = 4 << 20
	zstdMaxWindow = 8 << 20
)

// encoders holds zstd encoders for encryptTo to use again. Making one costs
// several MiB, and a large file in chunks needs one per chunk. At most one
// per worker is in use at a time.
var encoders sync.Pool

// encryptTo streams r through zstd and age into new files inside rt, and
// returns the SHA-256 of the plaintext that was read and the files written,
// in order. The first file is rel (a slash path inside rt). With limit above
// zero, the compressed stream moves on to a new part under a random name
// each time a part holds limit bytes; with zero it all goes into rel.
//
// Every part is written to a temporary file and renamed into place only once
// the whole stream has been written, so a failure leaves nothing behind.
// Every write goes through rt (os.Root), which refuses any path that leads
// outside the repository, for example through a symlinked objects/ folder
// committed by someone else.
func encryptTo(rt *os.Root, rel string, r io.Reader, recipients []age.Recipient, limit int64) (plainSHA string, parts []cachePart, err error) {
	pw := &partWriter{rt: rt, recipients: recipients, limit: limit}
	defer func() {
		if err != nil {
			pw.abort()
		}
	}()
	if err := pw.open(rel); err != nil {
		return "", nil, err
	}
	zw, ok := encoders.Get().(*zstd.Encoder)
	if !ok {
		zw, err = zstd.NewWriter(nil,
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(zstdWindow),
			zstd.WithLowerEncoderMem(true))
		if err != nil {
			return "", nil, err
		}
	}
	zw.Reset(pw)
	h := sha256.New()
	if _, err := io.Copy(zw, io.TeeReader(r, h)); err != nil {
		zw.Close()
		return "", nil, err
	}
	if err := zw.Close(); err != nil {
		return "", nil, err
	}
	// Only an encoder that closed cleanly is used again, and it lets go of
	// pw first.
	zw.Reset(nil)
	encoders.Put(zw)
	if err := pw.closePart(); err != nil {
		return "", nil, err
	}
	if err := pw.commit(); err != nil {
		return "", nil, err
	}
	return sum(h), pw.parts, nil
}

// partWriter writes a compressed stream into one age file after another.
// Each part is a complete age file, so `salt check` accepts it and restore
// decrypts each on its own. Only one part is open at a time.
type partWriter struct {
	rt         *os.Root
	recipients []age.Recipient
	limit      int64       // compressed bytes per part, or 0 for no limit
	parts      []cachePart // final names, and sizes once closed
	tmps       []string    // temporary names, in step with parts
	f          *os.File    // the open part, or nil
	aw         io.WriteCloser
	n          int64 // compressed bytes in the open part
}

// open starts a new part that will be renamed to rel.
func (w *partWriter) open(rel string) error {
	dir := filepath.Dir(filepath.FromSlash(rel))
	if err := w.rt.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpName, err := tempName(dir)
	if err != nil {
		return err
	}
	f, err := w.rt.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	w.parts = append(w.parts, cachePart{Object: rel})
	w.tmps = append(w.tmps, tmpName)
	w.f, w.n = f, 0
	w.aw, err = age.Encrypt(f, w.recipients...)
	return err
}

// Write fills the open part up to the limit, then starts the next one. A
// part is started only when there is something to put in it, so a stream
// that ends exactly at a limit has no empty last part.
func (w *partWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.f == nil {
			name, err := objectName(true, "")
			if err != nil {
				return written, err
			}
			if err := w.open(name); err != nil {
				return written, err
			}
		}
		chunk := p
		if w.limit > 0 {
			chunk = p[:min(int64(len(p)), w.limit-w.n)]
		}
		n, err := w.aw.Write(chunk)
		written += n
		w.n += int64(n)
		if err != nil {
			return written, err
		}
		p = p[n:]
		if w.limit > 0 && w.n >= w.limit {
			if err := w.closePart(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// closePart finishes the open part, if any, and records its size and
// last-modified time.
func (w *partWriter) closePart() error {
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	if err := w.aw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Read once the file is closed, as some file systems date a file when
	// it is closed. Renaming the part into place keeps both.
	fi, err := w.rt.Lstat(w.tmps[len(w.tmps)-1])
	if err != nil {
		return err
	}
	part := &w.parts[len(w.parts)-1]
	part.CipherSize, part.ModTime = fi.Size(), fi.ModTime().UnixNano()
	return nil
}

// commit moves every part into place. The first part goes last, so a file
// already at its name is replaced only once the other parts are in place.
func (w *partWriter) commit() error {
	for i := len(w.tmps) - 1; i >= 0; i-- {
		if err := w.rt.Rename(w.tmps[i], filepath.FromSlash(w.parts[i].Object)); err != nil {
			return err
		}
	}
	return nil
}

// abort closes the open part and removes every temporary file. A part that
// commit already renamed is no longer at its temporary name, so it stays;
// the index never names it, and the next seal removes it.
func (w *partWriter) abort() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	for _, tmp := range w.tmps {
		w.rt.Remove(tmp)
	}
}

// tempName returns a fresh temporary file name in dir.
func tempName(dir string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".salt-tmp-"+hex.EncodeToString(b)), nil
}

// decryptStream opens the objects (slash paths inside rt) holding one
// compressed stream of at most plain bytes of plaintext, for reading it. The
// first is opened now, so a missing file or a wrong key is reported here;
// each later one is opened only once the one before it is used up and
// closed. An object that takes the ciphertext past what plain bytes can
// make (see maxCipher) is refused with errOversized before it is decrypted.
// Call the returned function when done.
func decryptStream(rt *os.Root, objects []string, ids []age.Identity, plain int64) (io.Reader, func(), error) {
	pr := &partReader{rt: rt, ids: ids, names: objects, left: maxCipher(plain, len(objects))}
	if err := pr.next(); err != nil {
		return nil, nil, err
	}
	zr, err := zstd.NewReader(pr,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(zstdMaxWindow))
	if err != nil {
		pr.close()
		return nil, nil, err
	}
	return zr, func() { zr.Close(); pr.close() }, nil
}

// errOversized means an object holds more ciphertext than the plaintext the
// index records could ever make. Someone who can push may have replaced it.
var errOversized = errors.New("its encrypted file is larger than the file it holds could make it")

// cipherOverhead is what each object may add beyond its share of plain/64
// (see maxCipher): its age header, which is about 100 bytes for each of the
// at most 1,000 or so keys recipients.txt can hold, and its zstd frame.
const cipherOverhead = 256 << 10

// maxCipher is the most ciphertext n objects holding plain bytes of
// plaintext can take. zstd adds at most about plain/128 to data that does
// not compress, and age 16 bytes in every 64 KiB, so plain/64 is enough for
// both.
func maxCipher(plain int64, n int) int64 {
	extra := int64(n) * cipherOverhead
	if plain > (math.MaxInt64-extra)/2 {
		return math.MaxInt64
	}
	return plain + plain/64 + extra
}

// partReader reads the decrypted parts one after another as one stream.
type partReader struct {
	rt    *os.Root
	ids   []age.Identity
	names []string // parts not yet opened
	left  int64    // ciphertext the parts not yet opened may hold
	f     *os.File // the open part, or nil
	r     io.Reader
}

// next closes the open part and opens the next one.
func (p *partReader) next() error {
	p.close()
	name := p.names[0]
	p.names = p.names[1:]
	f, fi, err := regular.Open(p.rt.OpenFile, filepath.FromSlash(name))
	if err != nil {
		// The error's own path is the whole name from the index, so it is
		// named through clip instead, keeping the cause for errors.Is.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return fmt.Errorf("%s: %w", clip(path.Base(name)), err)
	}
	if fi.Size() > p.left {
		f.Close()
		return fmt.Errorf("%s: %w", clip(path.Base(name)), errOversized)
	}
	p.left -= fi.Size()
	ar, err := age.Decrypt(f, p.ids...)
	if err != nil {
		f.Close()
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return fmt.Errorf("%s: none of your keys can decrypt this backup", clip(path.Base(name)))
		}
		return fmt.Errorf("%s: %w", clip(path.Base(name)), err)
	}
	p.f, p.r = f, ar
	return nil
}

func (p *partReader) Read(b []byte) (int, error) {
	for {
		if p.r == nil {
			if len(p.names) == 0 {
				return 0, io.EOF
			}
			if err := p.next(); err != nil {
				return 0, err
			}
		}
		n, err := p.r.Read(b)
		if errors.Is(err, io.EOF) {
			p.close()
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

// close closes the open part, if any. It always forgets the part's reader,
// so Read moves on to the next part rather than reading a finished one again.
func (p *partReader) close() {
	if p.f != nil {
		p.f.Close()
	}
	p.f, p.r = nil, nil
}

// hashFile streams a regular file through SHA-256.
func hashFile(path string) (string, int64, error) {
	f, _, err := regular.Open(os.OpenFile, path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return sum(h), n, nil
}

func sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
