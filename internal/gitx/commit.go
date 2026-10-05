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
// stops git, which may be waiting on something the person set up.
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
	err = proc.Run(ctx, exec.CommandContext(ctx, "git", commitArgs(dir, msg)...))
	var failed *proc.Error
	if errors.As(err, &failed) {
		failed.Program = "git commit"
	}
	return err
}

// commitArgs returns the git arguments CommitStaged commits with. The commit
// is never signed, whatever commit.gpgsign says: it holds only ciphertext,
// the index in it is signed with salt's own key, and a signing key that asks
// for a passphrase would stop a scheduled backup every time.
func commitArgs(dir, msg string) []string {
	return Args(dir, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", msg)
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

// RemoteTimeout is the longest each git command that talks to origin may
// take. git also gives up on a transfer that stalls (see stallLimit), but an
// SSH connection can hang without git noticing.
var RemoteTimeout = 2 * time.Hour

// stallLimit makes git stop an HTTP transfer slower than 1 KiB/s for a
// minute, which by default it waits on for ever.
var stallLimit = []string{"-c", "http.lowSpeedLimit=1024", "-c", "http.lowSpeedTime=60"}

// ErrRemoteMoved means origin's branch holds a commit this machine did not
// push, so pushing would overwrite it.
var ErrRemoteMoved = errors.New("origin's branch is at a commit this machine has not pushed, such as a backup pushed from another machine, so salt will not overwrite it. Check what is there before backing up again. Each machine needs its own backup repo or branch")

// Head returns the commit checked out in the repository at dir.
func Head(dir string) (string, error) {
	return Run(dir, "rev-parse", "--verify", "HEAD")
}

// Lease reads where origin's copy of the branch checked out in the
// repository at dir is, and returns that commit for Push to lease to, or ""
// when origin has no such branch. It refuses with ErrRemoteMoved unless the
// commit is in known, already held by the local branch, or nothing, so
// nothing there is lost. known lists the commits this machine pushed, or
// tried to push, there. The remote-tracking branch never counts, since a
// fetch moves it to whatever another machine pushed. Call Lease before
// rewriting the branch's history: the branch then still holds the commit
// this machine last pushed, and any pushed by hand. Cancelling ctx stops
// git, and so does RemoteTimeout.
func Lease(ctx context.Context, dir string, known []string) (string, error) {
	branch, err := Branch(dir)
	if err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	out, err := remote(ctx, dir, "git ls-remote", "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	return leaseFor(remoteTip(out, ref), known, func(tip string) bool {
		_, err := Run(dir, "merge-base", "--is-ancestor", tip, "HEAD")
		return err == nil
	})
}

// Push pushes the checked-out branch of the repository at dir to the same
// branch on origin, replacing history salt prune rewrote, but only while
// origin's branch is still at lease, as Lease returned it ("" for no
// branch): --force-with-lease=ref:lease. A backup pushed from elsewhere
// since Lease looked is never lost, even if something fetched into the repo
// meanwhile. git never waits for a password to be typed, and errors never
// show the credentials origin's URL may hold. Cancelling ctx stops git, and
// so does RemoteTimeout.
func Push(ctx context.Context, dir, lease string) error {
	branch, err := Branch(dir)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
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
// transfer that stalls is stopped, the command is stopped after
// RemoteTimeout, and errors never show the credentials origin's URL may
// hold.
func remote(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, RemoteTimeout)
	defer cancel()
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
		err = fmt.Errorf("%s took longer than %v, so salt stopped it", name, RemoteTimeout)
	}
	return stdout.String(), err
}
