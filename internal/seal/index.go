package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

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

// maxIndexSize bounds how much decrypted index salt will read into memory.
const maxIndexSize = 256 << 20

func (ix *Index) marshal() ([]byte, string, error) {
	b, err := json.Marshal(ix)
	if err != nil {
		return nil, "", err
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:]), nil
}

func writeIndex(root string, b []byte, recipients []age.Recipient) error {
	_, err := encryptTo(filepath.Join(root, repo.IndexFile), bytes.NewReader(b), recipients)
	return err
}

// ReadIndex decrypts and validates a repository's index.
func ReadIndex(root string, ids []age.Identity) (*Index, error) {
	r, closeFn, err := decryptStream(filepath.Join(root, repo.IndexFile), ids)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	b, err := io.ReadAll(io.LimitReader(r, maxIndexSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	if len(b) > maxIndexSize {
		return nil, fmt.Errorf("index is larger than %d bytes", maxIndexSize)
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if ix.Version != repo.FormatVersion {
		return nil, fmt.Errorf("index version %d is not supported", ix.Version)
	}
	for i, e := range ix.Entries {
		p, err := repo.CleanPath(e.Path)
		if err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		ix.Entries[i].Path = p
		if e.Symlink == "" {
			if _, err := repo.CleanPath(e.Object); err != nil {
				return nil, fmt.Errorf("index: object for %s: %w", p, err)
			}
		}
	}
	return &ix, nil
}
