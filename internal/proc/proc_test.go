package proc

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// exec copies stderr with io.Copy, which must not get past the limit.
func TestLimitedBufferThroughCopy(t *testing.T) {
	b := &LimitedBuffer{Max: 5}
	big := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 1<<20))}
	if n, err := io.Copy(b, big); n != 1<<20 || err != nil {
		t.Fatalf("Copy = %d, %v", n, err)
	}
	if b.String() != "xxxxx" {
		t.Fatalf("kept %d bytes", len(b.String()))
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &LimitedBuffer{Max: 5}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if b.String() != "abcde" {
		t.Fatalf("kept %q", b.String())
	}
}

func TestLimitedBufferLines(t *testing.T) {
	for _, tt := range []struct {
		max    int
		writes []string
		want   string
	}{
		{100, []string{"one\n", "two\n"}, "one\ntwo"},
		{100, []string{"no newline at the end"}, "no newline at the end"},
		// Exactly full is not cut.
		{8, []string{"one\ntwo\n"}, "one\ntwo"},
		// A token cut part way is left out with its line.
		{12, []string{"fatal: x\n", "token ghp_secret\n"}, "fatal: x"},
		{12, []string{"fatal: x\ntoken ghp_secret\n"}, "fatal: x"},
		// A first line cut part way leaves nothing.
		{4, []string{"ghp_secret\n"}, ""},
		// Writes after the limit is reached still count as cut.
		{4, []string{"abc\n", "d"}, "abc"},
	} {
		b := &LimitedBuffer{Max: tt.max}
		for _, w := range tt.writes {
			b.Write([]byte(w))
		}
		if got := b.Lines(); got != tt.want {
			t.Errorf("Lines after %q with max %d = %q, want %q", tt.writes, tt.max, got, tt.want)
		}
	}
}

func TestError(t *testing.T) {
	cause := errors.New("exit status 1")
	err := error(&Error{Program: "pg_dump", Err: cause, Stderr: "connection refused"})
	if err.Error() != "pg_dump: exit status 1: connection refused" {
		t.Fatalf("Error() = %q", err)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Error does not unwrap to its cause")
	}
	// A program that says nothing gets no empty ": " on the end.
	if err := (&Error{Program: "pg_dump", Err: cause}); err.Error() != "pg_dump: exit status 1" {
		t.Fatalf("with no error output, Error() = %q", err)
	}
}

// Only a program's exit can be from a signal; an error starting it is not.
func TestStoppedBySignalNeedsAnExit(t *testing.T) {
	if stoppedBySignal(errors.New("fork/exec x: permission denied")) {
		t.Fatal("an error starting a program counted as stopped by a signal")
	}
}
