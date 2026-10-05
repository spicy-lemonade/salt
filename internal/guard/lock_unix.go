//go:build unix

package guard

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive lock on f without waiting, or returns
// ErrLocked when another open file holds it.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
