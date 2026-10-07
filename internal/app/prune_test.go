package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
)

func TestPruneReportsWhatItDropped(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	e.git.prune = &prune.Result{Kept: 6, Days: 5, Dropped: 12}
	e.ui.out.Reset()
	if err := e.app.Prune(e.root, 5); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.git.pruneDays, []int{5}) {
		t.Fatalf("git asked to prune with %v", e.git.pruneDays)
	}
	out := e.ui.out.String()
	for _, want := range []string{
		"salt: kept 6 backup(s) from the last 5 day(s) with a change and dropped 12 other(s)",
		"push with `git push --force-with-lease`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPruneWithNothingToDrop(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	e.git.prune = &prune.Result{Kept: 3, Days: 2}
	e.ui.out.Reset()
	if err := e.app.Prune(e.root, 5); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	if !strings.Contains(out, "nothing to prune: all 3 backup(s) are from the last 5 day(s) with a change") || strings.Contains(out, "force") {
		t.Fatalf("output:\n%s", out)
	}
}

// A failed clean-up comes after the branch has moved, so every kept backup
// is complete. It is a warning, not a failure, so a backup script still goes
// on to push.
func TestPruneWarnsAboutAFailedCleanup(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	e.git.prune = &prune.Result{Kept: 1, Days: 1, Dropped: 1}
	e.git.pruneErr = fmt.Errorf("%w: disk full", prune.ErrCleanup)
	e.ui.out.Reset()
	if err := e.app.Prune(e.root, 1); err != nil {
		t.Fatalf("Prune = %v, want only a warning", err)
	}
	out := e.ui.out.String()
	for _, want := range []string{
		"dropped 1 other(s)",
		"--force-with-lease",
		"salt: warning: the old backups were dropped, but removing them from the local repo failed: disk full; they take up space on this machine until the next prune removes them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Any other error alongside a result is still a failure.
func TestPruneFailsOnOtherErrorsWithAResult(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	broken := errors.New("git broke")
	e.git.prune = &prune.Result{Kept: 1, Days: 1}
	e.git.pruneErr = broken
	if err := e.app.Prune(e.root, 1); !errors.Is(err, broken) {
		t.Fatalf("Prune = %v", err)
	}
}

func TestPruneRefusals(t *testing.T) {
	// Not a salt repo, and not a git repo: git is never asked.
	e := newEnv(t)
	if err := e.app.Prune(t.TempDir(), 5); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("prune of a non-salt folder: %v", err)
	}
	healthyRepo(t, e)
	os.RemoveAll(filepath.Join(e.root, ".git"))
	if err := e.app.Prune(e.root, 5); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("prune outside git: %v", err)
	}
	os.MkdirAll(filepath.Join(e.root, ".git"), 0o755)

	// Keys changed since this machine approved them: history that shows
	// the change must not be rewritten.
	addAttackerKey(t, e.root)
	if err := e.app.Prune(e.root, 5); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("prune after a key was added: %v", err)
	}
	// Never approved on this machine.
	e.app.TrustDir = t.TempDir()
	if err := e.app.Prune(e.root, 5); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("prune of an unapproved repo: %v", err)
	}
	if len(e.git.pruneDays) != 0 {
		t.Fatalf("git asked to prune %d time(s) despite the refusals", len(e.git.pruneDays))
	}

	// git's own refusals come back unchanged, with nothing printed.
	e2 := newEnv(t)
	healthyRepo(t, e2)
	e2.git.pruneErr = prune.ErrNotPrunable
	e2.ui.out.Reset()
	if err := e2.app.Prune(e2.root, 5); !errors.Is(err, prune.ErrNotPrunable) || e2.ui.out.Len() != 0 {
		t.Fatalf("Prune = %v, output %q", err, e2.ui.out.String())
	}
}
