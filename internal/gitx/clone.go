package gitx

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// sshForm matches git's SSH form user@host:path, as GitHub shows it
// (git@github.com:you/backup.git). The host has no "/" so a local path such
// as a/b@c:d is never taken for it.
var sshForm = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:.+$`)

const httpsPrefix = "https://"

// IsRemote reports whether s is a backup repo's URL rather than a folder: an
// https URL or the SSH form user@host:path.
func IsRemote(s string) bool {
	return hasHTTPS(s) || sshForm.MatchString(s)
}

func hasHTTPS(s string) bool {
	return len(s) > len(httpsPrefix) && strings.EqualFold(s[:len(httpsPrefix)], httpsPrefix)
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
// dir, which must be empty. Cancelling ctx stops git. Errors never show the
// credentials remote may hold.
func Clone(ctx context.Context, remote, dir string) error {
	// "--" stops a URL starting with "-" from being read as an option.
	cmd := exec.CommandContext(ctx, "git", Args(dir, "clone", "--depth", "1", "--single-branch", "--no-tags", "--quiet", "--", remote, dir)...)
	err := proc.Run(ctx, cmd)
	var failed *proc.Error
	if errors.As(err, &failed) {
		failed.Program = "git clone " + RedactURL(remote)
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
