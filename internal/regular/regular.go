// Package regular opens files salt reads without ever waiting on something
// that is not a file.
package regular

import (
	"errors"
	"io/fs"
	"os"
)

// ErrNotRegular means a path names something other than a regular file, such
// as a named pipe, a device or a folder.
var ErrNotRegular = errors.New("not a regular file")

// Open opens name for reading with open, which is os.OpenFile or an os.Root's
// OpenFile, and returns the file and its details. A path checked before it is
// opened can become a named pipe in between, and opening a named pipe waits
// for ever for a writer. So the file is opened without waiting, checked once
// it is open, and only then set to wait as a file normally does. Anything but
// a regular file is closed again and refused with ErrNotRegular.
//
// Open is the last check, for a path that changed after its caller looked.
// Callers check the type first where they can: opening a named pipe, even
// without waiting, lets a tool waiting to write to it go on.
func Open(open func(string, int, fs.FileMode) (*os.File, error), name string) (*os.File, fs.FileInfo, error) {
	f, err := open(name, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = &fs.PathError{Op: "open", Path: f.Name(), Err: ErrNotRegular}
	}
	if err == nil {
		err = setBlocking(f)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}
