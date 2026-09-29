// Package repo describes the layout of a salt backup repository.
//
//	.salt/format.json     public: layout version and options
//	.salt/recipients.txt  public: age recipients every file is encrypted to
//	.salt/key.age         passphrase-wrapped identity (passphrase recovery only)
//	index.age             encrypted index: real paths, hashes, modes
//	objects/xx/….age      file contents when paths are encrypted (default)
//	files/<path>.age      file contents with --plain-paths
package repo

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

const (
	Dir            = ".salt"
	FormatFile     = ".salt/format.json"
	RecipientsFile = ".salt/recipients.txt"
	KeyFile        = ".salt/key.age"
	IndexFile      = "index.age"
	ObjectsDir     = "objects"
	FilesDir       = "files"

	FormatVersion = 1

	RecoveryPhrase     = "phrase"
	RecoveryPassphrase = "passphrase"
)

// Format is .salt/format.json.
type Format struct {
	Version      int    `json:"version"`
	EncryptPaths bool   `json:"encrypt_paths"`
	Recovery     string `json:"recovery"`
}

// Public lists the paths that may be committed without encryption. Everything
// else in a salt repo must be age ciphertext.
var Public = map[string]bool{
	"README.md":      true,
	"LICENSE":        true,
	".gitignore":     true,
	".gitattributes": true,
	FormatFile:       true,
	RecipientsFile:   true,
}

// ErrNotInitialised means the directory has no .salt/format.json.
var ErrNotInitialised = errors.New("not a salt repository (no " + FormatFile + "); run `salt init` first")

// Repo is an initialised salt repository on disk.
type Repo struct {
	Root       string
	Format     Format
	Recipients []age.Recipient
	// RecipientStrings are the public keys as written in recipients.txt.
	RecipientStrings []string
}

// Open loads a salt repository.
func Open(root string) (*Repo, error) {
	b, err := os.ReadFile(filepath.Join(root, FormatFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialised
	}
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: root}
	if err := json.Unmarshal(b, &r.Format); err != nil {
		return nil, fmt.Errorf("%s: %w", FormatFile, err)
	}
	if r.Format.Version != FormatVersion {
		return nil, fmt.Errorf("%s: format version %d is not supported by this salt; upgrade salt", FormatFile, r.Format.Version)
	}
	f, err := os.Open(filepath.Join(root, RecipientsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rcpt, err := age.ParseX25519Recipient(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", RecipientsFile, err)
		}
		r.Recipients = append(r.Recipients, rcpt)
		r.RecipientStrings = append(r.RecipientStrings, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(r.Recipients) == 0 {
		return nil, fmt.Errorf("%s has no recipients", RecipientsFile)
	}
	return r, nil
}

// Write creates .salt/format.json and .salt/recipients.txt. It writes through
// os.Root, so a symlink in the repo can't lead the files outside it.
func Write(root string, f Format, recipients []string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := rt.WriteFile(FormatFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	body := "# age public keys. Every file in this repo is encrypted to all of them.\n" +
		strings.Join(recipients, "\n") + "\n"
	return rt.WriteFile(RecipientsFile, []byte(body), 0o644)
}

// CleanPath validates a relative slash path from an index and returns it
// cleaned. It rejects anything that could escape the restore directory.
func CleanPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	return c, nil
}
