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
	"strconv"

	"github.com/spicy-lemonade/salt/internal/private"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// Pin is what was approved for one backup repo.
type Pin struct {
	Recipients   []string `json:"recipients"` // sorted
	EncryptPaths bool     `json:"encrypt_paths"`
	// Recovery is how the key is recovered (see repo.Format). A pin saved
	// by a salt that did not record it has none, and any is accepted.
	Recovery string `json:"recovery,omitempty"`
}

// For returns the pin describing r as it is now.
func For(r *repo.Repo) Pin {
	rs := slices.Clone(r.RecipientStrings)
	slices.Sort(rs)
	return Pin{Recipients: rs, EncryptPaths: r.Format.EncryptPaths, Recovery: r.Format.Recovery}
}

// ErrNotApproved means this machine has never approved the repo.
var ErrNotApproved = errors.New("not approved on this machine")

// Store keeps pins as 0600 files in Dir, one per repo, named by the repo's
// real path, so a repo reached through a symlink has the same pin.
type Store struct{ Dir string }

// paths returns where the pin for the repo at root is kept, and where an
// older salt, which named it by the repo's absolute path, kept it.
func (s Store) paths(root string) (path, older string, err error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	name := func(p string) string {
		sum := sha256.Sum256([]byte(p))
		return filepath.Join(s.Dir, hex.EncodeToString(sum[:8])+".json")
	}
	return name(real), name(abs), nil
}

// Load returns the approved pin for the repo at root. A pin an older salt
// saved under the repo's absolute path is read when there is none under its
// real path.
func (s Store) Load(root string) (Pin, error) {
	p, older, err := s.paths(root)
	if err != nil {
		return Pin{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) && older != p {
		p = older
		b, err = os.ReadFile(p)
	}
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
	p, older, err := s.paths(root)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return err
	}
	if err := private.Write(p, append(b, '\n')); err != nil {
		return err
	}
	// An approval an older salt saved under the path it was given is
	// replaced by this one. Losing this removal only leaves it unread.
	if older != p {
		os.Remove(older)
	}
	return nil
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
	if approved.Recovery != "" && approved.Recovery != got.Recovery {
		out = append(out, fmt.Sprintf("recovery changed from %s to %s", recoveryWord(approved.Recovery), recoveryWord(got.Recovery)))
	}
	return out
}

// recoveryWord names a recovery method. One salt does not know, which only
// someone who can push could have written, is quoted.
func recoveryWord(method string) string {
	switch method {
	case repo.RecoveryPhrase:
		return "a recovery phrase"
	case repo.RecoveryPassphrase:
		return "a passphrase"
	}
	return strconv.QuoteToGraphic(method)
}

func pathWord(encrypt bool) string {
	if encrypt {
		return "hidden"
	}
	return "visible"
}
