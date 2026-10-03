package seal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// cache is salt's local memory of what it sealed last time, so unchanged
// files keep their existing ciphertext (age output is randomised, so
// re-encrypting would change every file every night). It holds plaintext
// hashes and real paths, so it is never committed and is written 0600.
type cache struct {
	// Key ties the cache to a set of recipients and layout; if either
	// changes, everything is re-encrypted.
	Key       string                `json:"key"`
	IndexSHA  string                `json:"index_sha"`
	IndexSize int64                 `json:"index_size"`
	Files     map[string]cacheEntry `json:"files"`
}

type cacheEntry struct {
	SHA256     string `json:"sha256"`
	Object     string `json:"object"`
	CipherSize int64  `json:"cipher_size"`
}

func cacheKey(recipients []string, encryptPaths bool) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(recipients, "\n")))
	if encryptPaths {
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cachePath(dir, repoRoot string) (string, error) {
	return repoFile(dir, repoRoot, "seal-", ".json")
}

// repoFile names the local file holding what salt keeps for the repo at
// repoRoot.
func repoFile(dir, repoRoot, prefix, ext string) (string, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256([]byte(abs))
	return filepath.Join(dir, prefix+hex.EncodeToString(s[:8])+ext), nil
}

// copyKeyLen is the length of a copy key: 128 random bits in hex.
const copyKeyLen = 32

// CopyKey returns a random secret salt keeps for the repo at repoRoot, made
// the first time it is asked for. Database copies use it in place of a key
// their program would otherwise make up anew each time (see
// source.CopyOptions). It is kept 0600 next to the change cache and never
// committed. If it is lost, a new one only means one more commit.
func CopyKey(dir, repoRoot string) (string, error) {
	p, err := repoFile(dir, repoRoot, "copykey-", "")
	if err != nil {
		return "", err
	}
	// A damaged key is replaced.
	if b, err := os.ReadFile(p); err == nil && len(b) == copyKeyLen {
		if _, err := hex.DecodeString(string(b)); err == nil {
			return string(b), nil
		}
	}
	raw := make([]byte, copyKeyLen/2)
	rand.Read(raw)
	key := hex.EncodeToString(raw)
	if err := writePrivate(p, []byte(key)); err != nil {
		return "", err
	}
	return key, nil
}

func loadCache(path, key string) *cache {
	empty := &cache{Key: key, Files: map[string]cacheEntry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var c cache
	if json.Unmarshal(b, &c) != nil || c.Key != key || c.Files == nil {
		return empty
	}
	return &c
}

func (c *cache) save(path string) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writePrivate(path, b)
}

// writePrivate replaces the file at path with b, readable only by its owner.
func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultCacheDir is ~/Library/Caches/salt, ~/.cache/salt, etc.
func DefaultCacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", errors.New("no user cache directory; set $XDG_CACHE_HOME or $HOME")
	}
	return filepath.Join(d, "salt"), nil
}
