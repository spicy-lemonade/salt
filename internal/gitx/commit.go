package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

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

// PushTimeout is the longest a push, with its check of origin, may take.
// git also gives up on a transfer that stalls (see stallLimit), but an SSH
// connection can hang without git noticing.
var PushTimeout = 2 * time.Hour

// stallLimit makes git stop an HTTP transfer slower than 1 KiB/s for a
// minute, which by default it waits on for ever.
var stallLimit = []string{"-c", "http.lowSpeedLimit=1024", "-c", "http.lowSpeedTime=60"}

// ErrRemoteMoved means origin's branch holds a commit this machine did not
// push, so pushing would overwrite it.
var ErrRemoteMoved = errors.New("origin's branch is at a commit this machine has not pushed, such as a backup pushed from another machine, so salt will not overwrite it. Check what is there and bring it into this repo before backing up again")

// Head returns the commit checked out in the repository at dir.
func Head(dir string) (string, error) {
	return Run(dir, "rev-parse", "--verify", "HEAD")
}

// Push pushes the checked-out branch of the repository at dir to the same
// branch on origin, replacing history salt prune rewrote. It first reads
// where origin's branch is, and pushes only if that is a commit in known,
// a commit the local branch already holds, or nothing. known lists the
// commits this machine pushed, or tried to push, there; when it is empty,
// the remote-tracking branch stands in for it. The push is then leased to
// that exact commit (--force-with-lease=ref:commit), so a backup pushed from
// elsewhere in the meantime is never lost, even if something fetched into
// the repo since. git never waits for a password to be typed, and errors
// never show the credentials origin's URL may hold. Cancelling ctx stops
// git, and so does PushTimeout.
func Push(ctx context.Context, dir string, known []string) error {
	branch, err := Branch(dir)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if len(known) == 0 {
		if tracking, err := Run(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
			known = []string{tracking}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, PushTimeout)
	defer cancel()
	out, err := remote(ctx, dir, "git ls-remote", "ls-remote", "origin", ref)
	if err != nil {
		return err
	}
	lease, err := leaseFor(remoteTip(out, ref), known, func(tip string) bool {
		_, err := Run(dir, "merge-base", "--is-ancestor", tip, "HEAD")
		return err == nil
	})
	if err != nil {
		return err
	}
	_, err = remote(ctx, dir, "git push", "push", "--quiet", "--force-with-lease="+ref+":"+lease, "origin", ref+":"+ref)
	return err
}

// leaseFor returns the commit origin's branch must still be at for the push
// to go ahead: tip, which is "" when origin has no such branch. tip must be
// in known or held by the local branch, as ancestor reports, so nothing
// there is lost.
func leaseFor(tip string, known []string, ancestor func(tip string) bool) (string, error) {
	if tip == "" || slices.Contains(known, tip) || ancestor(tip) {
		return tip, nil
	}
	return "", ErrRemoteMoved
}

// remoteTip returns the commit ls-remote output gives for ref, or "".
func remoteTip(out, ref string) string {
	for line := range strings.Lines(out) {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && name == ref {
			return sha
		}
	}
	return ""
}

// maxRemoteOutput caps what ls-remote may print for one branch.
const maxRemoteOutput = 64 << 10

// remote runs a git command that talks to origin, named name in errors, and
// returns its output. git never waits for a password to be typed, an HTTP
// transfer that stalls is stopped, and errors never show the credentials
// origin's URL may hold.
func remote(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", Args(dir, append(slices.Clone(stallLimit), args...)...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	stdout := &proc.LimitedBuffer{Max: maxRemoteOutput}
	cmd.Stdout = stdout
	err := proc.Run(ctx, cmd)
	var failed *proc.Error
	switch {
	case errors.As(err, &failed):
		failed.Program = name
		failed.Stderr = hideCredentials(failed.Stderr, Remote(dir))
	case errors.Is(err, context.DeadlineExceeded):
		err = fmt.Errorf("%s took longer than %v, so salt stopped it", name, PushTimeout)
	}
	return stdout.String(), err
}
