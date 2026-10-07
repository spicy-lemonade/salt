package gitx

import (
	"errors"
	"strings"
	"testing"
)

// A push goes ahead only when origin's branch is missing, at a commit this
// machine pushed or tried to push, or at one the local branch holds, and is
// then leased to exactly that commit.
func TestLeaseFor(t *testing.T) {
	held := func(tip string) bool { return tip == "old" }
	for _, tc := range []struct {
		tip   string
		known []string
		want  string
		err   error
	}{
		{"", nil, "", nil},
		{"", []string{"a"}, "", nil},
		{"a", []string{"x", "a"}, "a", nil},
		{"old", nil, "old", nil},
		{"theirs", []string{"a"}, "", ErrRemoteMoved},
		{"theirs", nil, "", ErrRemoteMoved},
	} {
		got, err := leaseFor(tc.tip, tc.known, held)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("leaseFor(%q, %v) = %q, %v, want %q, %v", tc.tip, tc.known, got, err, tc.want, tc.err)
		}
	}
}

func TestRemoteTip(t *testing.T) {
	out := "aaaa\trefs/heads/x/refs/heads/main\nbbbb\trefs/heads/main\n"
	if got := remoteTip(out, "refs/heads/main"); got != "bbbb" {
		t.Errorf("remoteTip = %q", got)
	}
	if got := remoteTip("", "refs/heads/main"); got != "" {
		t.Errorf("remoteTip of nothing = %q", got)
	}
}

// salt's commits are never signed, whatever commit.gpgsign says, and still
// run with hooks, compression and the delta search off.
func TestCommitArgs(t *testing.T) {
	got := strings.Join(commitArgs("/repo", "salt backup"), " ")
	want := "-c core.hooksPath=/dev/null -c core.looseCompression=0 -c pack.compression=0 -c pack.window=0 -C /repo -c commit.gpgsign=false commit --quiet -m salt backup"
	if got != want {
		t.Fatalf("commitArgs = %q, want %q", got, want)
	}
}

func TestDiskUsage(t *testing.T) {
	if n, err := diskUsage("5001451\n"); err != nil || n != 5001451 {
		t.Fatalf("diskUsage = %d, %v", n, err)
	}
	for _, bad := range []string{"", "lots", "-1", "12 34"} {
		if _, err := diskUsage(bad); err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Errorf("diskUsage(%q) = %v, want an error", bad, err)
		}
	}
}
