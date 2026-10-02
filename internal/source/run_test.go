package source

import (
	"io"
	"strings"
	"testing"
)

// exec copies stderr with io.Copy, which must not get past the limit.
func TestLimitedBufferThroughCopy(t *testing.T) {
	b := &limitedBuffer{max: 5}
	big := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 1<<20))}
	if n, err := io.Copy(b, big); n != 1<<20 || err != nil {
		t.Fatalf("Copy = %d, %v", n, err)
	}
	if b.String() != "xxxxx" {
		t.Fatalf("kept %d bytes", len(b.String()))
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 5}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if b.String() != "abcde" {
		t.Fatalf("kept %q", b.String())
	}
}
