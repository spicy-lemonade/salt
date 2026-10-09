// Package trust remembers, on this machine, which keys and settings a backup
// repo was approved with.
//
// The repo's own .salt/recipients.txt and .salt/format.json are public and
// can be edited by anyone who can push to the repo. Without a local copy to
// compare against, someone could add their own key or turn off hidden file
// names, and the next seal would quietly follow along. Seal checks the repo
// against the approved copy and refuses if they differ.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/spicy-lemonade/salt/internal/repo"
)

// Pin is what was approved for one backup repo.
type Pin struct {
	Recipients   []string `json:"recipients"` // sorted
	EncryptPaths bool     `json:"encrypt_paths"`
}

// For returns the pin describing r as it is now.
func For(r *repo.Repo) Pin {
	rs := slices.Clone(r.RecipientStrings)
	slices.Sort(rs)
	return Pin{Recipients: rs, EncryptPaths: r.Format.EncryptPaths}
}

// ErrNotApproved means this machine has never approved the repo.
var ErrNotApproved = errors.New("not approved on this machine")

// Store keeps pins as 0600 files in Dir, one per repo path.
type Store struct{ Dir string }

func (s Store) path(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:8])+".json"), nil
}

// Load returns the approved pin for the repo at root.
func (s Store) Load(root string) (Pin, error) {
	p, err := s.path(root)
	if err != nil {
		return Pin{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Pin{}, ErrNotApproved
	}
	if err != nil {
		return Pin{}, err
	}
	var pin Pin
	if err := json.Unmarshal(b, &pin); err != nil {
		return Pin{}, fmt.Errorf("%s: %w", p, err)
	}
	// Every repo has a key, so an approval with none is broken.
	if len(pin.Recipients) == 0 {
		return Pin{}, fmt.Errorf("%s: no keys", p)
	}
	return pin, nil
}

// Save approves pin for the repo at root.
func (s Store) Save(root string, pin Pin) error {
	p, err := s.path(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Diff describes, in plain words, how got differs from the approved pin.
// It returns nothing when they match.
func Diff(approved, got Pin) []string {
	var out []string
	for _, r := range got.Recipients {
		if !slices.Contains(approved.Recipients, r) {
			out = append(out, "key added: "+r)
		}
	}
	for _, r := range approved.Recipients {
		if !slices.Contains(got.Recipients, r) {
			out = append(out, "key removed: "+r)
		}
	}
	if approved.EncryptPaths != got.EncryptPaths {
		out = append(out, fmt.Sprintf("file names changed from %s to %s", pathWord(approved.EncryptPaths), pathWord(got.EncryptPaths)))
	}
	return out
}

func pathWord(encrypt bool) string {
	if encrypt {
		return "hidden"
	}
	return "visible"
}
