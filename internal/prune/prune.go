// Package prune keeps only a backup repo's recent snapshots.
//
// Every backup commit holds a complete snapshot: index.age plus every
// encrypted object. A snapshot can therefore be dropped by rewriting history
// so that the oldest commit kept becomes the first one. The commits kept are
// copied exactly, with the same tree, author, committer, dates and message;
// only their parent changes. Each kept backup restores exactly as before.
//
// Days are counted on the repo, not per file: a day counts when the repo has
// a commit dated that day, which happens when anything in it changed. A day
// with no change makes no commit and is skipped, so keeping 5 days can reach
// further back than 5 calendar days. Every backup on the latest day is kept,
// and only the last backup of each earlier day, so backups made many times a
// day do not multiply what history holds.
package prune

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/spicy-lemonade/salt/internal/gitx"
)

// DefaultKeepDays is how many days with a change are kept by default.
const DefaultKeepDays = 5

// Git is what pruning asks of git. RealGit implements it; tests use a fake.
type Git interface {
	Toplevel(dir string) (string, error)
	IsShallow(dir string) (bool, error)
	// CurrentBranch returns gitx.ErrDetached when HEAD is detached.
	CurrentBranch(dir string) (string, error)
	// FirstParentLog lists HEAD's first-parent line, newest first.
	FirstParentLog(dir string) ([]gitx.Commit, error)
	CatCommit(dir, sha string) ([]byte, error)
	HashCommit(dir string, raw []byte) (string, error)
	UpdateRef(dir, ref, newSHA, oldSHA, reason string) error
	// ReclaimSpace deletes what prune dropped from branch (see
	// gitx.ReclaimSpace).
	ReclaimSpace(dir, branch string) error
}

// RealGit runs git through gitx (hooks disabled).
type RealGit struct{}

func (RealGit) Toplevel(dir string) (string, error)      { return gitx.Toplevel(dir) }
func (RealGit) IsShallow(dir string) (bool, error)       { return gitx.IsShallow(dir) }
func (RealGit) CurrentBranch(dir string) (string, error) { return gitx.CurrentBranch(dir) }
func (RealGit) FirstParentLog(dir string) ([]gitx.Commit, error) {
	return gitx.FirstParentLog(dir)
}
func (RealGit) CatCommit(dir, sha string) ([]byte, error) { return gitx.CatCommit(dir, sha) }
func (RealGit) HashCommit(dir string, raw []byte) (string, error) {
	return gitx.HashCommit(dir, raw)
}
func (RealGit) UpdateRef(dir, ref, newSHA, oldSHA, reason string) error {
	return gitx.UpdateRef(dir, ref, newSHA, oldSHA, reason)
}
func (RealGit) ReclaimSpace(dir, branch string) error { return gitx.ReclaimSpace(dir, branch) }

// Result summarises a prune.
type Result struct {
	Kept    int // commits kept
	Days    int // days with a change among the commits kept
	Dropped int // commits dropped from the branch
}

// ErrNotPrunable means the repo is not in a state salt will rewrite.
var ErrNotPrunable = errors.New("not pruning")

// ErrCleanup means the old backups were dropped from the branch, but git
// could not delete them from the local repo.
var ErrCleanup = errors.New("the old backups were dropped, but removing them from the local repo failed")

// Select returns the indexes of the commits to keep, newest first, and how
// many days they fall on. It keeps every commit on the latest day with a
// commit, and only the newest commit of each earlier day, over the keepDays
// most recent days with a commit. It always keeps the newest commit. Days
// are taken in branch order, so a commit with an odd clock still ends the
// window where it sits rather than being moved.
func Select(commits []gitx.Commit, keepDays int) (keep []int, days int) {
	seen := map[string]bool{}
	for i, c := range commits {
		if seen[c.Day] {
			if c.Day == commits[0].Day {
				keep = append(keep, i)
			}
			continue
		}
		if len(seen) == keepDays {
			break
		}
		seen[c.Day] = true
		keep = append(keep, i)
	}
	return keep, len(seen)
}

// Run drops every commit Select does not keep from the branch checked out
// in the repo at root, then deletes them from the local repo. It changes
// nothing when there is nothing to drop.
func Run(g Git, root string, keepDays int) (*Result, error) {
	if keepDays < 1 {
		return nil, fmt.Errorf("%w: the number of days to keep must be 1 or more, got %d", ErrNotPrunable, keepDays)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := checkToplevel(g, root); err != nil {
		return nil, err
	}
	shallow, err := g.IsShallow(root)
	if err != nil {
		return nil, err
	}
	if shallow {
		return nil, fmt.Errorf("%w: %s is a shallow clone, so salt cannot see the dates of the older backups. Run `git -C %q fetch --unshallow` first",
			ErrNotPrunable, root, root)
	}
	branch, err := g.CurrentBranch(root)
	if errors.Is(err, gitx.ErrDetached) {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotPrunable, root, err)
	}
	if err != nil {
		return nil, err
	}
	commits, err := g.FirstParentLog(root)
	if err != nil {
		return nil, err
	}
	keep, days := Select(commits, keepDays)
	res := &Result{Kept: len(keep), Days: days, Dropped: len(commits) - len(keep)}
	if res.Dropped == 0 {
		return res, nil
	}

	// Copy the kept commits oldest first, each onto the copy before it.
	parent := ""
	for _, i := range slices.Backward(keep) {
		raw, err := g.CatCommit(root, commits[i].SHA)
		if err != nil {
			return nil, err
		}
		next, err := Reparent(raw, parent)
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", commits[i].SHA, err)
		}
		if parent, err = g.HashCommit(root, next); err != nil {
			return nil, err
		}
	}
	reason := fmt.Sprintf("salt prune: keep the last %d days with a change, one a day before the latest", keepDays)
	if err := g.UpdateRef(root, branch, parent, commits[0].SHA, reason); err != nil {
		return nil, err
	}
	if err := g.ReclaimSpace(root, branch); err != nil {
		return res, fmt.Errorf("%w: %v", ErrCleanup, err)
	}
	return res, nil
}

// checkToplevel refuses a root that is not the top of its git work tree, so
// a folder inside some other repository never has that repository rewritten.
func checkToplevel(g Git, root string) error {
	top, err := g.Toplevel(root)
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	got, err := filepath.EvalSymlinks(top)
	if err != nil {
		return err
	}
	if filepath.Clean(want) != filepath.Clean(got) {
		return fmt.Errorf("%w: %s is inside the git repository %s, not at its top; salt only rewrites the history of the repo it was set up in",
			ErrNotPrunable, root, top)
	}
	return nil
}

// Reparent returns the raw commit object with its parents replaced by parent
// ("" for none). Every other header and the message are kept byte for byte,
// apart from signatures (gpgsig, gpgsig-sha256 and mergetag), which no longer
// match once the parent changes.
func Reparent(raw []byte, parent string) ([]byte, error) {
	header, message, found := bytes.Cut(raw, []byte("\n\n"))
	if !found {
		return nil, errors.New("malformed commit: no blank line after the header")
	}
	lines := bytes.Split(header, []byte("\n"))
	if !bytes.HasPrefix(lines[0], []byte("tree ")) {
		return nil, errors.New("malformed commit: does not start with a tree")
	}
	var out bytes.Buffer
	out.Grow(len(raw))
	out.Write(lines[0])
	out.WriteByte('\n')
	if parent != "" {
		fmt.Fprintf(&out, "parent %s\n", parent)
	}
	dropping := false
	for _, l := range lines[1:] {
		if bytes.HasPrefix(l, []byte(" ")) { // continues the header before it
			if !dropping {
				out.Write(l)
				out.WriteByte('\n')
			}
			continue
		}
		name, _, _ := bytes.Cut(l, []byte(" "))
		switch string(name) {
		case "parent", "gpgsig", "gpgsig-sha256", "mergetag":
			dropping = true
			continue
		}
		dropping = false
		out.Write(l)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	out.Write(message)
	return out.Bytes(), nil
}
