package guard

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another salt holds the lock, such as last night's backup
// still running when tonight's starts.
var ErrLocked = errors.New("another salt is already working on this backup repo; wait for it to finish, or stop it, and try again")

// Lock takes the lock held in the file at path, made if missing, and returns
// a function that releases it. It never waits for another salt. The system
// releases the lock when the process ends, however it ends, so a crash never
// leaves it held.
func Lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
