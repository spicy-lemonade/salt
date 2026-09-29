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
	"os"
	"path"
	"path/filepath"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// zstd settings chosen for bounded memory rather than maximum ratio.
const (
	zstdWindow    = 4 << 20
	zstdMaxWindow = 8 << 20
)

// encryptTo streams r through zstd and age into a new file at rel (a slash
// path inside rt), written atomically. It returns the SHA-256 of the
// plaintext that was read.
//
// Every write goes through rt (os.Root), which refuses any path that leads
// outside the repository, for example through a symlinked objects/ folder
// committed by someone else.
func encryptTo(rt *os.Root, rel string, r io.Reader, recipients []age.Recipient) (plainSHA string, err error) {
	dst := filepath.FromSlash(rel)
	dir := filepath.Dir(dst)
	if err := rt.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmpName, err := tempName(dir)
	if err != nil {
		return "", err
	}
	tmp, err := rt.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			rt.Remove(tmpName)
		}
	}()
	aw, err := age.Encrypt(tmp, recipients...)
	if err != nil {
		return "", err
	}
	zw, err := zstd.NewWriter(aw,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(zstdWindow),
		zstd.WithLowerEncoderMem(true))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(zw, io.TeeReader(r, h)); err != nil {
		zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := aw.Close(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := rt.Rename(tmpName, dst); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tempName returns a fresh temporary file name in dir.
func tempName(dir string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".salt-tmp-"+hex.EncodeToString(b)), nil
}

// decryptStream opens the object at rel (a slash path inside rt) for reading
// plaintext. Close the returned closer when done.
func decryptStream(rt *os.Root, rel string, ids []age.Identity) (io.Reader, func(), error) {
	f, err := rt.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, nil, err
	}
	ar, err := age.Decrypt(f, ids...)
	if err != nil {
		f.Close()
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, nil, fmt.Errorf("%s: none of your keys can decrypt this backup", path.Base(rel))
		}
		return nil, nil, fmt.Errorf("%s: %w", path.Base(rel), err)
	}
	zr, err := zstd.NewReader(ar,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(zstdMaxWindow))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return zr, func() { zr.Close(); f.Close() }, nil
}

// hashFile streams a file through SHA-256.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
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
