// Package gitx runs git for salt with hooks disabled.
//
// salt must never trigger a hook: hooks run salt check, and a hook fired from
// inside salt is exactly the recursion that crashed the first attempt. Every
// git invocation goes through Args, which pins core.hooksPath to an empty
// location.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// noHooks is prepended to every git command salt runs.
var noHooks = []string{"-c", "core.hooksPath=/dev/null"}

// Args returns the full git argument list for a command run in dir.
func Args(dir string, args ...string) []string {
	out := make([]string, 0, len(noHooks)+2+len(args))
	out = append(out, noHooks...)
	out = append(out, "-C", dir)
	return append(out, args...)
}

// Run runs git in dir and returns trimmed stdout.
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", Args(dir, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
