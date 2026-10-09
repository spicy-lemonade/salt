//go:build unix && !aix

package regular

import (
	"os"

	"golang.org/x/sys/unix"
)

// nonBlock opens a named pipe at once, rather than waiting for a writer.
const nonBlock = unix.O_NONBLOCK

// setBlocking clears nonBlock from f, so a file system that honours it on a
// regular file never makes a read return early with nothing.
func setBlocking(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) { setErr = unix.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return setErr
}
