package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// sshForm matches git's SSH form user@host:path, as GitHub shows it
// (git@github.com:you/backup.git). The host has no "/" so a local path such
// as a/b@c:d is never taken for it.
var sshForm = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:.+$`)

// scheme matches the start of a URL such as https://host or ssh://host. A
// scheme has at least two letters, so a Windows drive (C://) is a folder.
var scheme = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]+)://.`)

// downloadSchemes are the URL schemes salt downloads a backup over. Both are
// encrypted, so a password or token in the URL is never sent in the clear.
var downloadSchemes = []string{"https", "ssh"}

// IsRemote reports whether s names a backup repo by URL rather than by folder:
// anything of the form scheme://, or the SSH form user@host:path. It returns
// an error for a URL with a scheme salt does not download over.
func IsRemote(s string) (bool, error) {
	m := scheme.FindStringSubmatch(s)
	if m == nil {
		return sshForm.MatchString(s), nil
	}
	if !slices.Contains(downloadSchemes, strings.ToLower(m[1])) {
		return true, fmt.Errorf("salt cannot download a backup from %s. Give an https or ssh URL, such as https://github.com/you/backup.git or git@github.com:you/backup.git, or the path of a folder", RedactURL(s))
	}
	return true, nil
}

// credentials matches the "user:password@" part of every URL in a text, such
// as an error message, and captures the scheme before it and the user name
// and password. It is matched by hand rather than with net/url so that a
// URL net/url refuses still has its credentials removed. A password holding
// "@" is matched whole, since the match runs to the last "@" before the
// path.
var credentials = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/?#\s'"]*)@`)

// RedactURL returns s without the user name and password that any URL in it
// may hold, so it can be shown. The user name goes too, since a GitHub token
// is often given in its place. The SSH form's user (git@) is not a secret
// and is kept.
func RedactURL(s string) string {
	return credentials.ReplaceAllString(s, "$1")
}

// Clone downloads only the latest commit of remote's default branch into
// dir, which must be empty. The URL is fetched from directly, never saved as
// a remote, so a download left behind by a salt that was killed outright
// holds no password or token from it. Cancelling ctx stops git. Errors never
// show the credentials remote may hold.
func Clone(ctx context.Context, remote, dir string) error {
	if err := cloneStep(ctx, dir, remote, nil, "init", "--quiet"); err != nil {
		return err
	}
	// "--" stops a URL starting with "-" from being read as an option.
	// --depth 1 fetches only the commit HEAD names.
	err := cloneStep(ctx, dir, remote, nil, "fetch", "--depth", "1", "--no-tags", "--quiet", "--", remote, "HEAD")
	if failed := (*proc.Error)(nil); errors.As(err, &failed) {
		// A remote with no default branch, such as a new, empty one, has
		// nothing to download, as git clone finds too. A fetch that was
		// stopped is not asked about.
		head := &proc.LimitedBuffer{Max: 1}
		if cloneStep(ctx, dir, remote, head, "ls-remote", "--", remote, "HEAD") == nil && head.String() == "" {
			return nil
		}
	}
	if err != nil {
		return err
	}
	return cloneStep(ctx, dir, remote, nil, "checkout", "--quiet", "--detach", "FETCH_HEAD")
}

// cloneStep runs one git command for Clone in dir, with its output going to
// stdout. Its errors name the command, with the URL for one that talks to
// remote, and never show the credentials remote may hold.
func cloneStep(ctx context.Context, dir, remote string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", Args(dir, args...)...)
	cmd.Stdout = stdout
	// ls-remote runs only after a fetch failed, which may have been a
	// password typed wrong, so it never asks for one again.
	if args[0] == "ls-remote" {
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	err := proc.Run(ctx, cmd)
	var failed *proc.Error
	if errors.As(err, &failed) {
		failed.Program = "git " + args[0]
		if slices.Contains(args, remote) {
			failed.Program += " " + RedactURL(remote)
		}
		failed.Stderr = hideCredentials(failed.Stderr, remote)
	}
	return err
}

// hideCredentials removes the user name and password from every URL in
// git's error output, and hides the password remote holds wherever else it
// appears, as given and with its %-escapes undone. A user name with no
// password is only removed from URLs, so a name that is also in the path,
// as in https://you@github.com/you/backup.git, still reads normally.
func hideCredentials(out, remote string) string {
	out = RedactURL(out)
	m := credentials.FindStringSubmatch(remote)
	if m == nil {
		return out
	}
	_, pass, _ := strings.Cut(m[2], ":")
	if pass == "" {
		return out
	}
	out = strings.ReplaceAll(out, pass, "***")
	if plain, err := url.PathUnescape(pass); err == nil && plain != "" {
		out = strings.ReplaceAll(out, plain, "***")
	}
	return out
}
