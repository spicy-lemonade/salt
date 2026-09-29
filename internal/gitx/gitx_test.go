package gitx

import (
	"slices"
	"testing"
)

func TestArgsDisablesHooks(t *testing.T) {
	got := Args("/repo", "commit", "-m", "x")
	want := []string{"-c", "core.hooksPath=/dev/null", "-C", "/repo", "commit", "-m", "x"}
	if !slices.Equal(got, want) {
		t.Fatalf("Args() = %q, want %q", got, want)
	}
}
