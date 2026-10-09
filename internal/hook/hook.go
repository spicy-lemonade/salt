// Package hook installs salt's git pre-commit hook.
package hook

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spicy-lemonade/salt/internal/regular"
)

// Marker identifies a hook salt wrote, so it can be updated but a user's own
// hook is never overwritten.
const Marker = "# installed by salt"

// Script calls salt by name, never by the path of the running binary: in a
// test that path is the test binary, which is how the first attempt recursed.
// Cron jobs run with a minimal PATH, so Homebrew's locations are appended.
// If salt cannot be found the commit is refused (fail closed).
const Script = `#!/bin/sh
` + Marker + `: refuses commits that contain unencrypted files.
PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin"
if ! command -v salt >/dev/null 2>&1; then
  echo "salt: not found on PATH; refusing commit so plaintext cannot slip through." >&2
  exit 1
fi
exec salt check
`

// maxHook is the most of a hook salt reads, far more than Script. A larger
// hook is not salt's.
const maxHook = 64 << 10

// ErrForeign means a pre-commit hook exists that salt did not write.
var ErrForeign = errors.New("a pre-commit hook already exists")

// Install writes the hook to path. It refuses to replace a hook salt did not
// write, returning ErrForeign; the caller should say where it is and tell the
// user to add `salt check` to it, as it does for a hook over maxHook bytes.
// Anything at path that is not a regular file, such as a named pipe, is
// refused without waiting on it.
func Install(path string) error {
	var b []byte
	f, _, err := regular.Open(os.OpenFile, path)
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(f, maxHook+1))
		f.Close()
	}
	switch {
	case err == nil:
		if len(b) > maxHook || !strings.Contains(string(b), Marker) {
			return ErrForeign
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(Script), 0o755)
}

// Installed reports whether path holds a hook that runs salt check. It never
// waits on something that is not a regular file, such as a named pipe, and
// reads at most maxHook bytes.
func Installed(path string) bool {
	f, _, err := regular.Open(os.OpenFile, path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHook+1))
	return err == nil && len(b) <= maxHook && strings.Contains(string(b), "salt check")
}
