//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/gitx"
)

// gitAtLeast reports whether `git version` output names major.minor or later.
func gitAtLeast(t *testing.T, out string, major, minor int) bool {
	t.Helper()
	f := strings.Fields(out)
	if len(f) < 3 {
		t.Fatalf("git version: %q", out)
	}
	v := strings.SplitN(f[2], ".", 3)
	if len(v) < 2 {
		t.Fatalf("git version: %q", out)
	}
	maj, err1 := strconv.Atoi(v[0])
	mnr, err2 := strconv.Atoi(v[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("git version: %q", out)
	}
	return maj > major || maj == major && mnr >= minor
}

// PushSize measures what a push would send with real git: everything when
// origin has nothing, and only the new objects once origin holds an earlier
// commit. A lease git does not have, or a cancelled context, is an error.
// It runs git in this process, so git is pointed at the test's home folder
// and no git config of the person's is read.
func TestPushSize(t *testing.T) {
	e := newEnv(t)
	t.Setenv("HOME", e.home)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if !gitAtLeast(t, e.must(e.home, "git", "version"), 2, 31) {
		t.Skip("git rev-list --disk-usage needs git 2.31 or later")
	}
	dir := filepath.Join(e.home, "repo")
	e.must(e.home, "git", "init", "-q", "-b", "main", dir)
	commit := func(name string, size int) string {
		writeNoise(t, filepath.Join(dir, name), size)
		e.must(dir, "git", "add", name)
		e.must(dir, "git", "commit", "-q", "-m", name)
		return strings.TrimSpace(e.must(dir, "git", "rev-parse", "HEAD"))
	}
	within := func(n, size int64) bool { return n >= size && n < size+64<<10 }
	ctx := context.Background()

	first := commit("a.bin", 3<<20)
	if n, err := gitx.PushSize(ctx, dir, ""); err != nil || !within(n, 3<<20) {
		t.Fatalf("PushSize with nothing on origin = %d, %v, want about 3 MiB", n, err)
	}
	commit("b.bin", 2<<20)
	if n, err := gitx.PushSize(ctx, dir, first); err != nil || !within(n, 2<<20) {
		t.Fatalf("PushSize past the first commit = %d, %v, want about 2 MiB", n, err)
	}
	if n, err := gitx.PushSize(ctx, dir, strings.Repeat("0", 40)); err == nil {
		t.Fatalf("PushSize with a lease git does not have = %d", n)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := gitx.PushSize(cancelled, dir, first); err == nil {
		t.Fatal("PushSize with a cancelled context succeeded")
	}
}
