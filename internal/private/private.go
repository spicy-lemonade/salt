// Package private writes files that only their owner can read, such as
// salt's caches, records and approvals on this machine.
package private

import (
	"os"
	"path/filepath"
)

// Write replaces the file at path with b, readable only by its owner,
// making its folder, owner-only, if it is missing. b is written to a new
// file under a random name beside path, which is never one already there,
// and then renamed over path, so a reader sees the old contents or the new,
// never part of them.
func Write(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails, harmlessly, once renamed
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
