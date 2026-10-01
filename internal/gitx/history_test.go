package gitx

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

var errGit = errors.New("git broke")

// fakeGit stands in for runGit. It answers each command from replies, keyed by
// the command's arguments joined with spaces, and fails the command named by
// fail. Every call is recorded.
type fakeGit struct {
	replies map[string]string
	fail    string
	calls   []string
	stdin   [][]byte
}

func (f *fakeGit) run(_ string, stdin []byte, _ int, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	f.stdin = append(f.stdin, stdin)
	if f.fail != "" && strings.HasPrefix(cmd, f.fail) {
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
		"rev-parse --git-dir":                 ".git\n",
		"symbolic-ref -q HEAD":                "refs/heads/main\n",
		"rev-parse --verify -q HEAD^{commit}": "aaaa\n",
		"log --first-parent --format=%H%x00%P%x00%cI HEAD": "aaaa\x00bbbb\x002026-09-02T06:00:00Z\n" +
			"bbbb\x00\x002026-09-01T06:00:00Z\n",
		"cat-file commit aaaa":             "tree 1\n\nmsg\n",
		"hash-object -t commit -w --stdin": "cccc\n",
		"for-each-ref --format=%(refname)": "refs/heads/main\nrefs/stash\nrefs/remotes/origin/main\n",
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

	// The stash's reflog holds the stash entries themselves, so it is never
	// emptied; HEAD's and every other ref's are.
	f.calls = nil
	if err := ReclaimSpace("/repo"); err != nil {
		t.Fatalf("ReclaimSpace = %v", err)
	}
	wantCalls := []string{
		"for-each-ref --format=%(refname)",
		"reflog expire --expire=now --expire-unreachable=now HEAD refs/heads/main refs/remotes/origin/main",
		"-c pack.window=0 -c pack.depth=0 -c gc.auto=0 gc --prune=now --quiet",
	}
	if !slices.Equal(f.calls, wantCalls) {
		t.Errorf("ReclaimSpace ran\n%q\nwant\n%q", f.calls, wantCalls)
	}
}

func TestHistoryEdgeAnswers(t *testing.T) {
	f := &fakeGit{replies: map[string]string{"rev-parse --is-shallow-repository": "false\n"}}
	useFake(t, f)
	if shallow, err := IsShallow("/repo"); err != nil || shallow {
		t.Errorf("IsShallow of a full clone = %v, %v", shallow, err)
	}
	// symbolic-ref exits 1 when HEAD is detached: no branch, and no error.
	f.fail = "symbolic-ref"
	if branch, err := CurrentBranch("/repo"); err != nil || branch != "" {
		t.Errorf("CurrentBranch with a detached HEAD = %q, %v", branch, err)
	}
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
		{"rev-parse --git-dir", func() error { _, err := CurrentBranch("/repo"); return err }, 1},
		{"log", func() error { _, err := FirstParentLog("/repo"); return err }, 2},
		{"cat-file", func() error { _, err := CatCommit("/repo", "aaaa"); return err }, 1},
		{"hash-object", func() error { _, err := HashCommit("/repo", []byte("x")); return err }, 1},
		{"update-ref", func() error { return UpdateRef("/repo", "refs/heads/main", "b", "a", "why") }, 1},
		{"for-each-ref", func() error { return ReclaimSpace("/repo") }, 1},
		{"reflog expire", func() error { return ReclaimSpace("/repo") }, 2},
		{"-c pack.window=0", func() error { return ReclaimSpace("/repo") }, 3},
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
