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

func TestBlobSpec(t *testing.T) {
	for _, tt := range []struct{ rev, path, want string }{
		{"", "x", ":0:x"},
		{"", "0:x", ":0:0:x"}, // must not read as stage 0 of "x"
		{"", "2:a/b", ":0:2:a/b"},
		{"HEAD", "0:x", "HEAD:0:x"},
	} {
		if got := BlobSpec(tt.rev, tt.path); got != tt.want {
			t.Errorf("BlobSpec(%q, %q) = %q, want %q", tt.rev, tt.path, got, tt.want)
		}
	}
}
