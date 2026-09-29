package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// Index is the encrypted table of contents (index.age). It is the only place
// real file paths are recorded when paths are encrypted.
type Index struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Entry is one file or symlink in a snapshot.
type Entry struct {
	Path    string `json:"path"`
	Object  string `json:"object,omitempty"` // repo-relative, regular files only
	SHA256  string `json:"sha256,omitempty"` // of the plaintext
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`              // permission bits
	Symlink string `json:"symlink,omitempty"` // target, symlinks only
}

// Limits on a decrypted index. A tampered index is only a few KB once
// compressed but could expand far beyond what salt should hold in memory, so
// it is read one entry at a time and refused past these limits. Seal refuses
// to write an index that would break them.
const (
	maxIndexSize   = 32 << 20 // bytes of decrypted JSON
	maxIndexString = 4096     // bytes in any path, object name or link target
)

// MaxIndexEntries is the most files and symlinks one backup can hold.
var MaxIndexEntries = 100_000

func (ix *Index) marshal() ([]byte, string, error) {
	b, err := json.Marshal(ix)
	if err != nil {
		return nil, "", err
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:]), nil
}

func writeIndex(root string, b []byte, recipients []age.Recipient) error {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	_, err = encryptTo(rt, repo.IndexFile, bytes.NewReader(b), recipients)
	return err
}

// ReadIndex decrypts and validates a repository's index. It decodes one entry
// at a time, so memory stays bounded however the index was crafted.
func ReadIndex(root string, ids []age.Identity) (*Index, error) {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	r, closeFn, err := decryptStream(rt, repo.IndexFile, ids)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	lr := &io.LimitedReader{R: r, N: maxIndexSize + 1}
	ix, err := decodeIndex(json.NewDecoder(lr))
	if lr.N <= 0 {
		return nil, fmt.Errorf("index is larger than %d bytes", maxIndexSize)
	}
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if ix.Version != repo.FormatVersion {
		return nil, fmt.Errorf("index version %d is not supported", ix.Version)
	}
	return ix, nil
}

func decodeIndex(dec *json.Decoder) (*Index, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	ix := &Index{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch key {
		case "version":
			if err := dec.Decode(&ix.Version); err != nil {
				return nil, err
			}
		case "entries":
			if err := decodeEntries(dec, ix); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unexpected field %v", key)
		}
	}
	return ix, expectDelim(dec, '}')
}

func decodeEntries(dec *json.Decoder, ix *Index) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	for dec.More() {
		if len(ix.Entries) >= MaxIndexEntries {
			return fmt.Errorf("more than %d entries", MaxIndexEntries)
		}
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return err
		}
		if err := validEntry(&e); err != nil {
			return err
		}
		ix.Entries = append(ix.Entries, e)
	}
	return expectDelim(dec, ']')
}

func validEntry(e *Entry) error {
	if len(e.Path) > maxIndexString || len(e.Object) > maxIndexString || len(e.Symlink) > maxIndexString {
		return fmt.Errorf("entry longer than %d bytes", maxIndexString)
	}
	p, err := repo.CleanPath(e.Path)
	if err != nil {
		return err
	}
	e.Path = p
	if e.Symlink == "" {
		if _, err := repo.CleanPath(e.Object); err != nil {
			return fmt.Errorf("object for %s: %w", p, err)
		}
	}
	return nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, t)
	}
	return nil
}
