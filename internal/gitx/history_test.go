package gitx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

var errGit = errors.New("git broke")

// exitError is a git that ran and exited with its code, as *exec.ExitError
// reports it.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// fakeGit stands in for runGit. It answers each command from replies, keyed by
// the command's arguments joined with spaces, and fails the command named by
// fail with failWith, or errGit. Every call is recorded.
type fakeGit struct {
	replies  map[string]string
	fail     string
	failWith error
	calls    []string
	stdin    [][]byte
}

func (f *fakeGit) run(_ string, stdin []byte, _ int, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	f.stdin = append(f.stdin, stdin)
	if f.fail != "" && strings.HasPrefix(cmd, f.fail) {
		if f.failWith != nil {
			return nil, fmt.Errorf("git %s: %w", cmd, f.failWith)
		}
		return nil, errGit
	}
	return []byte(f.replies[cmd]), nil
}

// useFake makes runGit the fake for one test.
func useFake(t *testing.T, f *fakeGit) {
	t.Helper()
	orig := runGit
	runGit = f.run
	t.Cleanup(func() { runGit = orig })
}

func TestHistoryAnswers(t *testing.T) {
	f := &fakeGit{replies: map[string]string{
		"rev-parse --show-toplevel":           "/repo\n",
		"rev-parse --is-shallow-repository":   "true\n",
		"symbolic-ref -q HEAD":                "refs/heads/main\n",
		"rev-parse --verify -q HEAD^{commit}": "aaaa\n",
		"log --first-parent --format=%H%x00%P%x00%cI HEAD": "aaaa\x00bbbb\x002026-09-02T06:00:00Z\n" +
			"bbbb\x00\x002026-09-01T06:00:00Z\n",
		"cat-file commit aaaa":             "tree 1\n\nmsg\n",
		"hash-object -t commit -w --stdin": "cccc\n",
		"for-each-ref --format=%(refname) refs/heads/main refs/remotes/origin/main": "refs/heads/main\nrefs/heads/main/old\nrefs/remotes/origin/main\n",
		"for-each-ref --format=%(refname) refs/heads/new refs/remotes/origin/new":   "refs/heads/new\n",
	}}
	useFake(t, f)

	if top, err := Toplevel("/repo"); err != nil || top != "/repo" {
		t.Errorf("Toplevel = %q, %v", top, err)
	}
	if shallow, err := IsShallow("/repo"); err != nil || !shallow {
		t.Errorf("IsShallow = %v, %v", shallow, err)
	}
	if branch, err := CurrentBranch("/repo"); err != nil || branch != "refs/heads/main" {
		t.Errorf("CurrentBranch = %q, %v", branch, err)
	}
	log, err := FirstParentLog("/repo")
	want := []Commit{{"aaaa", 1, "2026-09-02"}, {"bbbb", 0, "2026-09-01"}}
	if err != nil || !slices.Equal(log, want) {
		t.Errorf("FirstParentLog = %v, %v", log, err)
	}
	if raw, err := CatCommit("/repo", "aaaa"); err != nil || string(raw) != "tree 1\n\nmsg\n" {
		t.Errorf("CatCommit = %q, %v", raw, err)
	}
	if sha, err := HashCommit("/repo", []byte("tree 1\n\nmsg\n")); err != nil || sha != "cccc" {
		t.Errorf("HashCommit = %q, %v", sha, err)
	}
	if string(f.stdin[len(f.stdin)-1]) != "tree 1\n\nmsg\n" {
		t.Errorf("HashCommit sent %q to git", f.stdin[len(f.stdin)-1])
	}
	if err := UpdateRef("/repo", "refs/heads/main", "cccc", "aaaa", "why"); err != nil {
		t.Errorf("UpdateRef = %v", err)
	}
	if got := f.calls[len(f.calls)-1]; got != "update-ref -m why refs/heads/main cccc aaaa" {
		t.Errorf("UpdateRef ran %q", got)
	}

	// Only the reflogs that hold what prune dropped are emptied: HEAD's,
	// the branch's and origin's copy of it, when there is one. A ref below
	// the branch's name, or any other, keeps its reflog.
	for branch, expire := range map[string]string{
		"refs/heads/main": "HEAD refs/heads/main refs/remotes/origin/main",
		"refs/heads/new":  "HEAD refs/heads/new",
	} {
		f.calls = nil
		if err := ReclaimSpace("/repo", branch); err != nil {
			t.Fatalf("ReclaimSpace = %v", err)
		}
		short := strings.TrimPrefix(branch, "refs/heads/")
		wantCalls := []string{
			"for-each-ref --format=%(refname) " + branch + " refs/remotes/origin/" + short,
			"reflog expire --expire=now --expire-unreachable=now " + expire,
			"-c gc.auto=0 gc --prune=now --quiet",
		}
		if !slices.Equal(f.calls, wantCalls) {
			t.Errorf("ReclaimSpace ran\n%q\nwant\n%q", f.calls, wantCalls)
		}
	}
}

func TestHistoryEdgeAnswers(t *testing.T) {
	f := &fakeGit{replies: map[string]string{"rev-parse --is-shallow-repository": "false\n"}}
	useFake(t, f)
	if shallow, err := IsShallow("/repo"); err != nil || shallow {
		t.Errorf("IsShallow of a full clone = %v, %v", shallow, err)
	}
	// symbolic-ref exits 1 when HEAD is detached, and only then.
	f.fail, f.failWith = "symbolic-ref", exitError(1)
	if branch, err := CurrentBranch("/repo"); !errors.Is(err, ErrDetached) || branch != "" {
		t.Errorf("CurrentBranch with a detached HEAD = %q, %v", branch, err)
	}
	f.failWith = exitError(128)
	if _, err := CurrentBranch("/repo"); errors.Is(err, ErrDetached) || err == nil {
		t.Errorf("CurrentBranch when git fails = %v", err)
	}
	f.failWith = nil
	// No commits yet: nothing to list, and no error.
	f.fail = "rev-parse --verify"
	if log, err := FirstParentLog("/repo"); err != nil || log != nil {
		t.Errorf("FirstParentLog with no commits = %v, %v", log, err)
	}
}

// When git fails at any step, the error comes back and nothing after that
// step is run.
func TestHistoryGitFailures(t *testing.T) {
	for _, tt := range []struct {
		fail string
		call func() error
		ran  int // commands run, including the failing one
	}{
		{"rev-parse --show-toplevel", func() error { _, err := Toplevel("/repo"); return err }, 1},
		{"rev-parse --is-shallow-repository", func() error { _, err := IsShallow("/repo"); return err }, 1},
		{"symbolic-ref", func() error { _, err := CurrentBranch("/repo"); return err }, 1},
		{"log", func() error { _, err := FirstParentLog("/repo"); return err }, 2},
		{"cat-file", func() error { _, err := CatCommit("/repo", "aaaa"); return err }, 1},
		{"hash-object", func() error { _, err := HashCommit("/repo", []byte("x")); return err }, 1},
		{"update-ref", func() error { return UpdateRef("/repo", "refs/heads/main", "b", "a", "why") }, 1},
		{"for-each-ref", func() error { return ReclaimSpace("/repo", "refs/heads/main") }, 1},
		{"reflog expire", func() error { return ReclaimSpace("/repo", "refs/heads/main") }, 2},
		{"-c gc.auto=0", func() error { return ReclaimSpace("/repo", "refs/heads/main") }, 3},
	} {
		t.Run(tt.fail, func(t *testing.T) {
			f := &fakeGit{fail: tt.fail}
			useFake(t, f)
			if err := tt.call(); !errors.Is(err, errGit) {
				t.Fatalf("error = %v, want the git error", err)
			}
			if len(f.calls) != tt.ran {
				t.Fatalf("ran %q, want %d command(s)", f.calls, tt.ran)
			}
		})
	}
}
