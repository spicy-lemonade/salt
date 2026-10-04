package seal

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// Index is the encrypted table of contents (index.age). It is the only place
// real file paths are recorded when paths are encrypted.
type Index struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
	// Signature is the base64 Ed25519 signature of the index (see
	// indexDigest), made with the signing key derived from one of the
	// repo's age identities (keys.SigningKey). Anyone can encrypt to the
	// public key, so without it a planted index would decrypt like a real one.
	Signature string `json:"signature,omitempty"`
	// Unsigned is set by ReadIndex when it was allowed to accept an index
	// whose signature is missing or does not match. It is never written.
	Unsigned bool `json:"-"`
}

// Entry is one file or symlink in a snapshot.
type Entry struct {
	Path   string `json:"path"`
	Object string `json:"object,omitempty"` // repo-relative, regular files only
	// Parts are the objects after Object, in order, for a file sealed in
	// parts (see splitAbove). Restore joins them back into one stream.
	Parts   []string `json:"parts,omitempty"`
	SHA256  string   `json:"sha256,omitempty"` // of the plaintext
	Size    int64    `json:"size"`
	Mode    uint32   `json:"mode"`              // permission bits
	Symlink string   `json:"symlink,omitempty"` // target, symlinks only
	// MTime is the last-modified time in Unix nanoseconds, regular files
	// only. Zero means it was not recorded, as in backups made before salt
	// kept it, and restore then leaves the time of the restore.
	MTime int64 `json:"mtime,omitempty"`
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

// objects returns every object holding e's ciphertext, in order.
func (e Entry) objects() []string {
	return append([]string{e.Object}, e.Parts...)
}

// partsIndexVersion is the index version seal writes when a file is in
// parts, so an older salt, which would read only the first part, refuses the
// index instead. Every other index keeps repo.FormatVersion, so it stays
// byte for byte the same, signature included.
const partsIndexVersion = 2

func (ix *Index) marshal() ([]byte, string, error) {
	b, err := json.Marshal(ix)
	if err != nil {
		return nil, "", err
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:]), nil
}

// ErrNotSigned means the index is not signed by a key that comes from the
// keys that opened it. Someone who can push to the repo may have replaced it
// to plant files.
var ErrNotSigned = errors.New("the backup's index is not signed by your key")

// signatureContext keeps index signatures apart from anything else.
const signatureContext = "salt index signature v1\n"

// indexDigest hashes what an index's signature covers: its version, then
// each entry in order, as JSON. Seal and ReadIndex both add one value at a
// time, so checking a signature never needs a second copy of the index.
type indexDigest struct {
	h   hash.Hash
	enc *json.Encoder
}

func newIndexDigest() *indexDigest {
	h := sha512.New()
	h.Write([]byte(signatureContext))
	return &indexDigest{h: h, enc: json.NewEncoder(h)}
}

// add adds the index version or an entry. Encoding an int or an Entry into
// a hash cannot fail: an Entry holds only strings, numbers and a list of
// strings, and writing to a hash never returns an error.
func (d *indexDigest) add(v any) { d.enc.Encode(v) }

func (d *indexDigest) sum() []byte { return d.h.Sum(nil) }

// sign sets ix.Signature. Ed25519 signatures are deterministic, so an
// unchanged index signs to the same bytes and makes no commit.
func (ix *Index) sign(key ed25519.PrivateKey) {
	d := newIndexDigest()
	d.add(ix.Version)
	for _, e := range ix.Entries {
		e = stored(e)
		d.add(&e)
	}
	ix.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, d.sum()))
}

// stored returns e as ReadIndex will decode it, so the signature covers what
// is stored. JSON keeps a string that is not valid UTF-8, such as a file
// name on Linux, with U+FFFD in place of each bad byte. An Entry holds only
// strings, numbers and a list of strings, so neither step can fail.
func stored(e Entry) Entry {
	validParts := !slices.ContainsFunc(e.Parts, func(p string) bool { return !utf8.ValidString(p) })
	if validParts && utf8.ValidString(e.Path) && utf8.ValidString(e.Object) && utf8.ValidString(e.Symlink) && utf8.ValidString(e.SHA256) {
		return e
	}
	b, _ := json.Marshal(e)
	var out Entry
	json.Unmarshal(b, &out)
	return out
}

// checkSignature reports whether ix.Signature signs digest with the signing
// key of one of ids.
func checkSignature(ix *Index, digest []byte, ids []age.Identity) error {
	if ix.Signature == "" {
		return fmt.Errorf("%w: it has no signature", ErrNotSigned)
	}
	sig, err := base64.StdEncoding.DecodeString(ix.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: its signature is malformed", ErrNotSigned)
	}
	for _, id := range ids {
		x, ok := id.(*age.X25519Identity)
		if !ok {
			continue
		}
		k := keys.SigningKey(x)
		if ed25519.Verify(k.Public().(ed25519.PublicKey), digest, sig) {
			return nil
		}
	}
	return fmt.Errorf("%w: its signature does not match", ErrNotSigned)
}

func writeIndex(root string, b []byte, recipients []age.Recipient) error {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	_, _, err = encryptTo(rt, repo.IndexFile, bytes.NewReader(b), recipients, 0)
	return err
}

// ReadIndex decrypts and validates a repository's index, and checks it is
// signed by the signing key of one of ids. It decodes one entry at a time, so
// memory stays bounded however the index was crafted. allowUnsigned accepts
// an index whose signature is missing or does not match, and marks it
// Unsigned; the files are still checked against it.
func ReadIndex(root string, ids []age.Identity, allowUnsigned bool) (*Index, error) {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	r, closeFn, err := decryptStream(rt, []string{repo.IndexFile}, ids)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	lr := &io.LimitedReader{R: r, N: maxIndexSize + 1}
	d := newIndexDigest()
	ix, err := decodeIndex(json.NewDecoder(newTokenLimitReader(lr, maxIndexJSONString, maxIndexToken)), d)
	if lr.N <= 0 {
		return nil, fmt.Errorf("index is larger than %d bytes", maxIndexSize)
	}
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if ix.Version != repo.FormatVersion && ix.Version != partsIndexVersion {
		return nil, fmt.Errorf("index version %d is not supported by this salt; upgrade salt", ix.Version)
	}
	if err := checkSignature(ix, d.sum(), ids); err != nil {
		if !allowUnsigned || !errors.Is(err, ErrNotSigned) {
			return nil, err
		}
		ix.Unsigned = true
	}
	return ix, nil
}

// decodeIndex decodes an index, adding its version and each entry to d as
// they are read, before validEntry cleans them.
func decodeIndex(dec *json.Decoder, d *indexDigest) (*Index, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	ix := &Index{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, decodeErr(err)
		}
		switch key {
		case "version":
			if err := dec.Decode(&ix.Version); err != nil {
				return nil, decodeErr(err)
			}
			d.add(ix.Version)
		case "entries":
			if err := decodeEntries(dec, ix, d); err != nil {
				return nil, err
			}
		case "signature":
			if err := dec.Decode(&ix.Signature); err != nil {
				return nil, decodeErr(err)
			}
		default:
			return nil, fmt.Errorf("unexpected field %s", clipToken(key))
		}
	}
	return ix, expectDelim(dec, '}')
}

func decodeEntries(dec *json.Decoder, ix *Index, d *indexDigest) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	for dec.More() {
		if len(ix.Entries) >= MaxIndexEntries {
			return fmt.Errorf("more than %d entries", MaxIndexEntries)
		}
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return decodeErr(err)
		}
		d.add(&e)
		if err := validEntry(&e); err != nil {
			return err
		}
		ix.Entries = append(ix.Entries, e)
	}
	return expectDelim(dec, ']')
}

// checkIndexEntries runs the checks ReadIndex makes on every entry, so seal
// never writes an index that restore would refuse.
func checkIndexEntries(entries []Entry) error {
	for _, e := range entries {
		if err := validEntry(&e); err != nil {
			return fmt.Errorf("cannot back up %s: %w", clip(e.Path), err)
		}
	}
	return nil
}

func validEntry(e *Entry) error {
	if len(e.Path) > maxIndexString || len(e.Object) > maxIndexString || len(e.Symlink) > maxIndexString || len(e.SHA256) > maxIndexString {
		return fmt.Errorf("entry longer than %d bytes", maxIndexString)
	}
	for _, part := range e.Parts {
		if len(part) > maxIndexString {
			return fmt.Errorf("entry longer than %d bytes", maxIndexString)
		}
	}
	// Messages name the path through clip: it can be up to maxIndexString
	// bytes, and repo.CleanPath's own error would quote all of it.
	p, err := repo.CleanPath(e.Path)
	if err != nil {
		return fmt.Errorf("unsafe path %s", clipQuoted(e.Path))
	}
	e.Path = p
	if e.Symlink != "" {
		if e.Object != "" || len(e.Parts) > 0 || e.SHA256 != "" {
			return fmt.Errorf("symlink %s must not have an object or hash", clip(p))
		}
		if e.MTime != 0 {
			return fmt.Errorf("symlink %s must not have a last-modified date", clip(p))
		}
		return nil
	}
	for _, obj := range e.objects() {
		if _, err := repo.CleanPath(obj); err != nil {
			return fmt.Errorf("object for %s: unsafe path %s", clip(p), clipQuoted(obj))
		}
	}
	if !isSHA256(e.SHA256) {
		return fmt.Errorf("hash for %s is not 64 hex characters", clip(p))
	}
	if e.Size < 0 {
		return fmt.Errorf("size for %s is negative", clip(p))
	}
	return nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// clip shortens a value from the index before it goes into an error message,
// so a crafted index can't print megabytes into logs or cron mail.
func clip(s string) string {
	const max = 40
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%q… (%d bytes)", strings.ToValidUTF8(s[:max], ""), len(s))
}

// clipQuoted is clip for a value that should be quoted even when short, such
// as a path that may be empty or contain spaces.
func clipQuoted(s string) string {
	if c := clip(s); c != s {
		return c
	}
	return strconv.Quote(s)
}

// clipToken is clip for a JSON token, without first copying a long string.
func clipToken(t json.Token) string {
	if s, ok := t.(string); ok {
		return clip(s)
	}
	return clip(fmt.Sprint(t))
}

// maxDecodeErr is the longest error from encoding/json passed on unchanged.
const maxDecodeErr = 160

// decodeErr shortens an error from encoding/json, which can quote part of the
// index in its message. Only encoding/json's own error types are shortened,
// so salt's messages (including the token limits) are never touched.
func decodeErr(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &syntaxErr) && !errors.As(err, &typeErr) {
		return err
	}
	if len(err.Error()) <= maxDecodeErr {
		return err
	}
	return &clippedError{err: err}
}

// clippedError keeps the original error for errors.Is and errors.As, but
// prints only the start of it.
type clippedError struct{ err error }

func (c *clippedError) Error() string {
	s := c.err.Error()
	return fmt.Sprintf("%s… (%d bytes)", strings.ToValidUTF8(s[:maxDecodeErr], ""), len(s))
}

func (c *clippedError) Unwrap() error { return c.err }

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return decodeErr(err)
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %s", want, clipToken(t))
	}
	return nil
}
