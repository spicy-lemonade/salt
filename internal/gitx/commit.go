package gitx

import (
	"context"
	"errors"
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
// message msg. It commits nothing when nothing is staged. Cancelling ctx
// stops git, which may be waiting on something the person set up, such as
// a key to sign commits with.
func CommitStaged(ctx context.Context, dir, msg string) error {
	// --quiet exits 1 when something is staged, without listing it.
	_, err := Run(dir, "diff", "--cached", "--quiet")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		return err
	}
	err = proc.Run(ctx, exec.CommandContext(ctx, "git", Args(dir, "commit", "--quiet", "-m", msg)...))
	var failed *proc.Error
	if errors.As(err, &failed) {
		failed.Program = "git commit"
	}
	return err
}

// ErrDetached means no branch is checked out, so there is no branch to
// commit a backup to or push.
var ErrDetached = errors.New("no branch is checked out (detached HEAD); check out the branch your backups go to")

// Branch returns the branch checked out in the repository at dir.
func Branch(dir string) (string, error) {
	branch, err := Run(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", ErrDetached
	}
	return branch, err
}

// Push pushes the checked-out branch of the repository at dir to the same
// branch on origin. --force-with-lease lets it replace history salt prune
// rewrote, but only if origin still holds what this machine last saw there,
// so a backup pushed from elsewhere is never lost. git never waits for a
// password to be typed, and errors never show the credentials origin's URL
// may hold. Cancelling ctx stops git.
func Push(ctx context.Context, dir string) error {
	branch, err := Branch(dir)
	if err != nil {
		return err
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
