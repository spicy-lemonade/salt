// Package gitx runs git for salt with hooks disabled.
//
// salt must never trigger a hook: hooks run salt check, and a hook fired from
// inside salt is exactly the recursion that crashed the first attempt. Every
// git invocation goes through Args, which pins core.hooksPath to an empty
// location.
package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// StagedPaths lists files added, copied, modified, renamed or type-changed in
// the index of the repository at dir.
func StagedPaths(dir string) ([]string, error) {
	cmd := exec.Command("git", Args(dir, "diff", "--cached", "--name-only", "-z", "--diff-filter=ACMRT")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff --cached: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// BlobHeads calls fn with the first n bytes of each path's blob at rev ("" for
// the index, "HEAD" for the last commit). It streams every blob through a
// single `git cat-file --batch`, so memory use stays bounded however large
// the files are. Paths containing a newline cannot be passed to cat-file and
// are reported with ok=false.
func BlobHeads(dir, rev string, paths []string, n int, fn func(path string, head []byte, ok bool) error) error {
	var batch []string
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			if err := fn(p, nil, false); err != nil {
				return err
			}
			continue
		}
		batch = append(batch, p)
	}
	if len(batch) == 0 {
		return nil
	}
	cmd := exec.Command("git", Args(dir, "cat-file", "--batch")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		w := bufio.NewWriter(stdin)
		for _, p := range batch {
			fmt.Fprintf(w, "%s:%s\n", rev, p)
		}
		w.Flush()
		stdin.Close()
	}()
	r := bufio.NewReader(stdout)
	head := make([]byte, n)
	var cbErr error
	for _, p := range batch {
		line, err := r.ReadString('\n')
		if err != nil {
			cbErr = fmt.Errorf("git cat-file: %w", err)
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "blob" {
			// "missing" or a non-blob (e.g. a submodule): never ciphertext.
			if cbErr = fn(p, nil, false); cbErr != nil {
				break
			}
			continue
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			cbErr = fmt.Errorf("git cat-file: bad size in %q", line)
			break
		}
		k := int(min(size, int64(n)))
		if _, err := io.ReadFull(r, head[:k]); err != nil {
			cbErr = err
			break
		}
		if _, err := io.CopyN(io.Discard, r, size-int64(k)+1); err != nil { // +1: trailing newline
			cbErr = err
			break
		}
		if cbErr = fn(p, head[:k], true); cbErr != nil {
			break
		}
	}
	if cbErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return cbErr
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// HookPath returns where git looks for the named hook in the repository at
// dir, honouring core.hooksPath.
//
// This is the one git call made without the hooksPath override, which would
// otherwise be reported back. rev-parse never runs hooks.
func HookPath(dir, name string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-path", "hooks/"+name).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-path: %w", err)
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

// TreePaths lists every file in the commit rev. It returns no paths and no
// error when rev does not exist yet (a repository with no commits).
func TreePaths(dir, rev string) ([]string, error) {
	if _, err := Run(dir, "rev-parse", "--verify", "-q", rev+"^{commit}"); err != nil {
		return nil, nil
	}
	cmd := exec.Command("git", Args(dir, "ls-tree", "-r", "-z", "--name-only", rev)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// LastCommitTime returns when HEAD was committed; ok is false with no commits.
func LastCommitTime(dir string) (t time.Time, ok bool, err error) {
	if _, err := Run(dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return time.Time{}, false, nil
	}
	out, err := Run(dir, "log", "-1", "--format=%ct")
	if err != nil {
		return time.Time{}, false, err
	}
	sec, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("git log: unexpected %q", out)
	}
	return time.Unix(sec, 0), true, nil
}

// Remote returns the URL of origin, or "" if there is none.
func Remote(dir string) string {
	out, err := Run(dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return out
}
