// Package hook installs salt's git pre-commit and pre-push hooks.
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

// Hook is a git hook salt installs.
type Hook struct {
	// Name is the hook's name, as git names its file.
	Name string
	// Runs is the salt command it runs, which a hook of the person's own
	// needs to run instead.
	Runs string
	// Refuses is what it refuses when they hold plaintext, such as commits.
	Refuses string
	// WarnIfMissing is set for a hook newer than salt init in some repos,
	// so doctor warns, rather than fails, when it is missing.
	WarnIfMissing bool
	// Script is the hook.
	Script string
}

// script returns a hook that runs the salt command runs, refusing one (such
// as "commit") when salt cannot be found. many is one in the plural. It calls salt by name, never
// by the path of the running binary: in a test that path is the test
// binary, which is how the first attempt recursed. Cron jobs run with a
// minimal PATH, so Homebrew's locations are appended. If salt cannot be
// found, it fails closed.
func script(one, many, runs string) string {
	return `#!/bin/sh
` + Marker + `: refuses ` + many + ` that contain unencrypted files.
PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin"
if ! command -v salt >/dev/null 2>&1; then
  echo "salt: not found on PATH; refusing ` + one + ` so plaintext cannot slip through." >&2
  exit 1
fi
exec ` + runs + `
`
}

var (
	// PreCommit refuses a commit that stages a file that is not encrypted.
	PreCommit = Hook{Name: "pre-commit", Runs: "salt check", Refuses: "commits", Script: script("commit", "commits", "salt check")}
	// PrePush refuses a push of commits holding a file that is not
	// encrypted, such as one a merge, a rebase or git commit --no-verify
	// made, which the pre-commit hook does not see. git gives it the
	// remote's name and URL, and the refs it pushes on its input.
	PrePush = Hook{Name: "pre-push", Runs: "salt check --pre-push", Refuses: "pushes", WarnIfMissing: true,
		Script: script("push", "pushes", `salt check --pre-push "$@"`)}
	// All are the hooks salt installs.
	All = []Hook{PreCommit, PrePush}
)

// maxHook is the most of a hook salt reads, far more than Script. A larger
// hook is not salt's.
const maxHook = 64 << 10

// ErrForeign means a hook exists that salt did not write.
var ErrForeign = errors.New("hook already exists")

// Install writes h to path. It refuses to replace a hook salt did not
// write, returning ErrForeign; the caller should say where it is and tell the
// user to add h.Runs to it, as it does for a hook over maxHook bytes.
// Anything at path that is not a regular file, such as a named pipe, is
// refused without waiting on it.
func Install(path string, h Hook) error {
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
	return os.WriteFile(path, []byte(h.Script), 0o755)
}

// Installed reports whether path holds a hook that runs h.Runs, and not only
// a longer command another hook runs, as "salt check --pre-push" is to
// "salt check". It never waits on something that is not a regular file,
// such as a named pipe, and reads at most maxHook bytes.
func Installed(path string, h Hook) bool {
	f, _, err := regular.Open(os.OpenFile, path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHook+1))
	if err != nil || len(b) > maxHook {
		return false
	}
	text := string(b)
	for _, o := range All {
		if o.Runs != h.Runs && strings.HasPrefix(o.Runs, h.Runs) {
			text = strings.ReplaceAll(text, o.Runs, "")
		}
	}
	return strings.Contains(text, h.Runs)
}
