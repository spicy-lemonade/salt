// Package seal encrypts a directory tree into a salt repository and restores
// it. All data is streamed: plaintext → zstd → age → file, with bounded
// buffers, so memory use does not grow with file size.
package seal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// zstd settings chosen for bounded memory rather than maximum ratio.
const (
	zstdWindow    = 4 << 20
	zstdMaxWindow = 8 << 20
)

// encryptTo streams r through zstd and age into a new file at dst, written
// atomically. It returns the SHA-256 of the plaintext that was read.
func encryptTo(dst string, r io.Reader, recipients []age.Recipient) (plainSHA string, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".salt-tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
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
	if err := tmp.Chmod(0o644); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decryptStream opens an object for reading plaintext. Close the returned
// closer when done.
func decryptStream(src string, ids []age.Identity) (io.Reader, func(), error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, err
	}
	ar, err := age.Decrypt(f, ids...)
	if err != nil {
		f.Close()
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, nil, fmt.Errorf("%s: none of your keys can decrypt this backup", filepath.Base(src))
		}
		return nil, nil, fmt.Errorf("%s: %w", filepath.Base(src), err)
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
