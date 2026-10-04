package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// StageAll stages every change in the repository at dir, removed files
// included.
func StageAll(dir string) error {
	_, err := Run(dir, "add", "--all")
	return err
}

// CommitStaged commits what is staged in the repository at dir with the
// message msg. It commits nothing when nothing is staged.
func CommitStaged(dir, msg string) error {
	// --quiet exits 1 when something is staged, without listing it.
	err := exec.Command("git", Args(dir, "diff", "--cached", "--quiet")...).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		return fmt.Errorf("git diff --cached: %w", err)
	}
	_, err = Run(dir, "commit", "--quiet", "-m", msg)
	return err
}

// Push pushes the checked-out branch of the repository at dir to the same
// branch on origin. --force-with-lease lets it replace history salt prune
// rewrote, but only if origin still holds what this machine last saw there,
// so a backup pushed from elsewhere is never lost. git never waits for a
// password to be typed, and errors never show the credentials origin's URL
// may hold. Cancelling ctx stops git.
func Push(ctx context.Context, dir string) error {
	branch, err := Run(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return errors.New("no branch is checked out, so there is nothing to push")
	}
	ref := "refs/heads/" + branch
	cmd := exec.CommandContext(ctx, "git", Args(dir, "push", "--force-with-lease", "--quiet", "origin", ref+":"+ref)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err = proc.Run(ctx, cmd)
	var failed *proc.Error
	if errors.As(err, &failed) {
		failed.Program = "git push"
		failed.Stderr = hideCredentials(failed.Stderr, Remote(dir))
	}
	return err
}
