// Package hook installs salt's git pre-commit hook.
package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// ErrForeign means a pre-commit hook exists that salt did not write.
var ErrForeign = errors.New("a pre-commit hook already exists")

// Install writes the hook to path. It refuses to replace a hook salt did not
// write; the caller should tell the user to add `salt check` to it.
func Install(path string) error {
	if b, err := os.ReadFile(path); err == nil && !strings.Contains(string(b), Marker) {
		return fmt.Errorf("%w at %s; add `salt check` to it so plaintext commits are refused", ErrForeign, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(Script), 0o755)
}

// Installed reports whether path holds a hook that runs salt check.
func Installed(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), "salt check")
}
