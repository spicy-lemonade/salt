package gitx

import (
	"errors"
	"slices"
	"strings"
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

func TestSplitZ(t *testing.T) {
	if got := splitZ([]byte("a\x00b c\x00line\nbreak\x00")); !slices.Equal(got, []string{"a", "b c", "line\nbreak"}) {
		t.Fatalf("splitZ = %q", got)
	}
	if got := splitZ(nil); len(got) != 0 {
		t.Fatalf("splitZ(nil) = %q", got)
	}
}

func TestReadCheckAttr(t *testing.T) {
	out := "index.age\x00text\x00unset\x00odd\nname.age\x00eol\x00crlf\x00"
	var got []Attr
	err := ReadCheckAttr(strings.NewReader(out), func(a Attr) error {
		got = append(got, a)
		return nil
	})
	want := []Attr{{"index.age", "text", "unset"}, {"odd\nname.age", "eol", "crlf"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ReadCheckAttr = %v, %v; want %v", got, err, want)
	}
	if err := ReadCheckAttr(strings.NewReader(""), func(Attr) error { return nil }); err != nil {
		t.Fatalf("empty output: %v", err)
	}
	for _, cut := range []string{"index.age", "index.age\x00", "index.age\x00text\x00", "index.age\x00text\x00unset"} {
		if err := ReadCheckAttr(strings.NewReader(cut), func(Attr) error { return nil }); err == nil {
			t.Errorf("truncated output %q accepted", cut)
		}
	}
	stop := errors.New("stop")
	if err := ReadCheckAttr(strings.NewReader(out), func(Attr) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
}
