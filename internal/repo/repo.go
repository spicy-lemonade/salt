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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	// GitHubFileLimit is the size above which GitHub refuses a pushed file.
	// Seal keeps every file it writes below it.
	GitHubFileLimit = 100 << 20

	RecoveryPhrase     = "phrase"
	RecoveryPassphrase = "passphrase"

	// maxSaltFile is the most ReadSaltFile reads. Salt writes its own files
	// far smaller, so a larger one was put there by someone else.
	maxSaltFile = 64 << 10
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

// missingError says salt's own file at a repo path is not there. It is
// fs.ErrNotExist.
type missingError string

func (e missingError) Error() string        { return string(e) + " is missing" }
func (e missingError) Is(target error) bool { return target == fs.ErrNotExist }

// ErrForeignSymlink means the backup repo contains a symlink salt did not
// create.
var ErrForeignSymlink = errors.New("backup repo contains a symlink salt did not create")

// ForeignSymlink is ErrForeignSymlink for the symlink at rel, a slash path in
// the repo.
func ForeignSymlink(rel string) error {
	return fmt.Errorf("%w at %s. Someone else added it. Remove it and check the repo's recent commits before backing up or restoring",
		ErrForeignSymlink, rel)
}

// Open loads a salt repository.
func Open(root string) (*Repo, error) {
	b, err := ReadSaltFile(root, FormatFile)
	if errors.Is(err, fs.ErrNotExist) {
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
	if b, err = ReadSaltFile(root, RecipientsFile); err != nil {
		return nil, err
	}
	// A bad key is named by its line number, not quoted, so the file's
	// contents never reach the terminal.
	n := 0
	for line := range bytes.Lines(b) {
		n++
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		s := string(line)
		rcpt, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("%s line %d is not an age public key", RecipientsFile, n)
		}
		r.Recipients = append(r.Recipients, rcpt)
		r.RecipientStrings = append(r.RecipientStrings, s)
	}
	if len(r.Recipients) == 0 {
		return nil, fmt.Errorf("%s has no recipients", RecipientsFile)
	}
	return r, nil
}

// ReadSaltFile reads name, one of salt's own files in .salt, from the repo at
// root. Someone who can push could commit it, or .salt, as a symlink. os.Root
// refuses a link that leads outside the repo but follows one that stays
// inside it, so ReadSaltFile refuses a symlink anywhere on the way, and
// anything at name that is not a regular file. It reads at most maxSaltFile
// bytes. A missing file is fs.ErrNotExist.
func ReadSaltFile(root, name string) ([]byte, error) {
	rt, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missingError(name)
	}
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	var fi fs.FileInfo
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err = rt.Lstat(filepath.FromSlash(p))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, missingError(name)
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, ForeignSymlink(p)
		}
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file. Check the repo's recent commits before backing up or restoring", name)
	}
	f, err := rt.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSaltFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSaltFile {
		return nil, fmt.Errorf("%s is larger than %d KiB, far larger than salt writes it. Check the repo's recent commits before backing up or restoring",
			name, maxSaltFile>>10)
	}
	return b, nil
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
