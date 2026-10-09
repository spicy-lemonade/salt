package regular

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A regular file opens, reads in full and is set to wait again, as a file
// opened with os.Open is. A symlink to one is followed, as os.Open does.
func TestOpenRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(p, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("notes.md", link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{p, link} {
		f, fi, err := Open(os.OpenFile, name)
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		// The flags are read through SyscallConn rather than Fd, which can
		// set a file to wait when Go made it non-blocking, so the check
		// never depends on that.
		var flags int
		var flagsErr error
		rc, err := f.SyscallConn()
		if err == nil {
			err = rc.Control(func(fd uintptr) { flags, flagsErr = unix.FcntlInt(fd, unix.F_GETFL, 0) })
		}
		if err = errors.Join(err, flagsErr); err != nil || flags&unix.O_NONBLOCK != 0 {
			t.Errorf("%s left non-blocking: flags %#x, %v", name, flags, err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil || string(b) != "hello" || fi.Size() != 5 || fi.Mode().Perm() != 0o640 {
			t.Errorf("%s read %q, %v, size %d, mode %v", name, b, err, fi.Size(), fi.Mode())
		}
	}
}

// A named pipe with no writer is refused at once rather than waited on, and
// so are a folder and a device. A missing file is fs.ErrNotExist.
func TestOpenRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	for _, name := range []string{pipe, dir, os.DevNull} {
		done := make(chan error, 1)
		go func() {
			f, _, err := Open(os.OpenFile, name)
			if err == nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			var pe *fs.PathError
			if !errors.Is(err, ErrNotRegular) || !errors.As(err, &pe) || pe.Path != name {
				t.Errorf("Open(%s) = %v, want ErrNotRegular naming it", name, err)
			}
		case <-time.After(10 * time.Second):
			// A writer coming and going lets the stuck open return, so it
			// does not outlive the test.
			if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}
			t.Fatalf("Open(%s) is still waiting", name)
		}
	}
	if _, _, err := Open(os.OpenFile, filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v, want fs.ErrNotExist", err)
	}
}

// Through an os.Root, a file inside opens and a symlink leading outside is
// refused before anything is read.
func TestOpenInRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inside"), []byte("in"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	rt, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	f, _, err := Open(rt.OpenFile, "inside")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if f, _, err := Open(rt.OpenFile, "link"); err == nil {
		f.Close()
		t.Fatal("opened a symlink leading outside the root")
	}
}
