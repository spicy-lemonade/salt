package gitx

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// MaxCommitSize is the largest commit object salt reads. Real commits are a
// few hundred bytes; the cap stops a crafted one from using much memory.
const MaxCommitSize = 1 << 20

// MaxLogSize caps the history FirstParentLog reads, about 300,000 commits.
const MaxLogSize = 32 << 20

// Commit is one commit on a branch's first-parent line.
type Commit struct {
	SHA     string
	Parents int
	// Day is the committer date in the committer's own time zone,
	// as YYYY-MM-DD, so it does not depend on this machine's time zone.
	Day string
}

// runCapped runs git in dir with stdin and returns its raw stdout, refusing
// output larger than limit bytes.
func runCapped(dir string, stdin []byte, limit int, args ...string) ([]byte, error) {
	cmd := exec.Command("git", Args(dir, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(limit)+1))
	if readErr == nil && len(out) > limit {
		readErr = fmt.Errorf("git %s: output is larger than %d bytes", args[0], limit)
	}
	if readErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, readErr
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// runGit runs every git command in this file. Unit tests replace it to make
// git fail at a chosen step, without starting git.
var runGit = runCapped

// gitLine runs git and returns its output with surrounding space trimmed.
func gitLine(dir string, args ...string) (string, error) {
	out, err := runGit(dir, nil, MaxLogSize, args...)
	return strings.TrimSpace(string(out)), err
}

// Toplevel returns the top folder of the git work tree containing dir.
func Toplevel(dir string) (string, error) {
	return gitLine(dir, "rev-parse", "--show-toplevel")
}

// IsShallow reports whether the repository at dir is a shallow clone, which
// is missing older history.
func IsShallow(dir string) (bool, error) {
	out, err := gitLine(dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// CurrentBranch returns the full name of the checked-out branch, such as
// "refs/heads/main", or "" when HEAD is detached.
func CurrentBranch(dir string) (string, error) {
	if _, err := gitLine(dir, "rev-parse", "--git-dir"); err != nil {
		return "", err
	}
	out, err := gitLine(dir, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", nil // exit 1: HEAD is detached
	}
	return out, nil
}

// FirstParentLog lists the commits on HEAD's first-parent line, newest first.
// It returns none when the branch has no commits yet.
func FirstParentLog(dir string) ([]Commit, error) {
	if _, err := gitLine(dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return nil, nil
	}
	out, err := runGit(dir, nil, MaxLogSize, "log", "--first-parent", "--format=%H%x00%P%x00%cI", "HEAD")
	if err != nil {
		return nil, err
	}
	return ParseLog(string(out))
}

// ParseLog reads FirstParentLog's git output: one line per commit holding the
// hash, the parent hashes and the strict ISO 8601 committer date, separated
// by NUL.
func ParseLog(out string) ([]Commit, error) {
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x00")
		if len(f) != 3 || f[0] == "" {
			return nil, fmt.Errorf("git log: unexpected line %q", line)
		}
		t, err := time.Parse(time.RFC3339, f[2])
		if err != nil {
			return nil, fmt.Errorf("git log: bad date in %q", line)
		}
		commits = append(commits, Commit{SHA: f[0], Parents: len(strings.Fields(f[1])), Day: t.Format(time.DateOnly)})
	}
	return commits, nil
}

// CatCommit returns the raw commit object sha, exactly as git stores it.
func CatCommit(dir, sha string) ([]byte, error) {
	return runGit(dir, nil, MaxCommitSize, "cat-file", "commit", sha)
}

// HashCommit writes raw as a commit object and returns its hash. git checks
// that raw is a well-formed commit first.
func HashCommit(dir string, raw []byte) (string, error) {
	out, err := runGit(dir, raw, 1024, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// UpdateRef points ref at newSHA, but only if it still points at oldSHA, so a
// commit made in the meantime is never lost.
func UpdateRef(dir, ref, newSHA, oldSHA, reason string) error {
	_, err := gitLine(dir, "update-ref", "-m", reason, ref, newSHA, oldSHA)
	return err
}

// ReclaimSpace deletes objects no longer reachable from any ref. It first
// empties the reflogs, which would otherwise keep dropped commits for 30 to
// 90 days, except the stash's, which holds the stash entries themselves.
// Delta search is off: ciphertext never compresses against itself, and the
// search is what makes repacking use memory.
func ReclaimSpace(dir string) error {
	refs, err := reflogRefs(dir)
	if err != nil {
		return err
	}
	args := append([]string{"reflog", "expire", "--expire=now", "--expire-unreachable=now"}, refs...)
	if _, err := gitLine(dir, args...); err != nil {
		return err
	}
	_, err = gitLine(dir, "-c", "pack.window=0", "-c", "pack.depth=0", "-c", "gc.auto=0",
		"gc", "--prune=now", "--quiet")
	return err
}

// reflogRefs lists HEAD and every ref except the stash.
func reflogRefs(dir string) ([]string, error) {
	out, err := gitLine(dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return nil, err
	}
	refs := []string{"HEAD"}
	for _, r := range strings.Split(out, "\n") {
		if r != "" && r != "refs/stash" {
			refs = append(refs, r)
		}
	}
	return refs, nil
}
