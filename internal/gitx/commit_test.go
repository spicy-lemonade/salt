package gitx

import (
	"errors"
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
