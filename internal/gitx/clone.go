package gitx

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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

// userinfo returns the "user:password@" part of an https URL, or "". It is
// found by hand rather than with net/url so that a URL net/url refuses still
// has its credentials removed.
func userinfo(s string) string {
	if !hasHTTPS(s) {
		return ""
	}
	authority := s[len(httpsPrefix):]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return ""
	}
	return authority[:at+1]
}

// RedactURL returns s without the user name and password an https URL may
// hold, so it can be shown. The user name goes too, since a GitHub token is
// often given in its place. The SSH form's user (git@) is not a secret and
// is kept.
func RedactURL(s string) string {
	if u := userinfo(s); u != "" {
		return httpsPrefix + s[len(httpsPrefix)+len(u):]
	}
	return s
}

// Clone downloads only the latest commit of url's default branch into dir,
// which must be empty. Cancelling ctx stops git. Errors never show the
// credentials url may hold.
func Clone(ctx context.Context, url, dir string) error {
	// "--" stops a url starting with "-" from being read as an option.
	cmd := exec.CommandContext(ctx, "git", Args(dir, "clone", "--depth", "1", "--single-branch", "--no-tags", "--quiet", "--", url, dir)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("salt needs git to download a backup; install git and try again")
	}
	if err == nil {
		return nil
	}
	out := strings.TrimSpace(stderr.String())
	if u := userinfo(url); u != "" {
		out = strings.ReplaceAll(out, u, "")
		// The secret is the password, or the user name when there is none,
		// since a token is often given in its place.
		user, pass, _ := strings.Cut(strings.TrimSuffix(u, "@"), ":")
		if secret := cmp.Or(pass, user); secret != "" {
			out = strings.ReplaceAll(out, secret, "***")
		}
	}
	return fmt.Errorf("git clone %s: %w: %s", RedactURL(url), err, out)
}
