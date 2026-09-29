package seal

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// readLimited reads s through a tokenLimitReader with strings capped at 4
// bytes and other tokens at 3, both in one go and one byte per Read, so
// escapes and tokens split across reads are covered.
func readLimited(t *testing.T, s string) []error {
	t.Helper()
	var errs []error
	for _, r := range []io.Reader{strings.NewReader(s), iotest.OneByteReader(strings.NewReader(s))} {
		got, err := io.ReadAll(newTokenLimitReader(r, 4, 3))
		if err == nil && string(got) != s {
			t.Fatalf("read %q, want %q", got, s)
		}
		errs = append(errs, err)
	}
	return errs
}

func TestTokenLimitReader(t *testing.T) {
	const tooLongString, tooLongValue = "string longer than 4 bytes", "value longer than 3 bytes"
	for name, tt := range map[string]struct {
		in, want string
	}{
		"exactly at the limit":          {`["abcd"]`, ""},
		"one byte over":                 {`["abcde"]`, tooLongString},
		"escaped quotes count as bytes": {`["\"\""]`, ""},
		"escaped quote does not end it": {`["a\"bc"]`, tooLongString},
		"escaped backslash then end":    {`["ab\\","abcd"]`, ""},
		"escaped backslash then over":   {`["ab\\","abcde"]`, tooLongString},
		"limit applies to every string": {`{"abcd":"abcd","x":"abcde"}`, tooLongString},
		"whitespace outside is free":    {`[` + strings.Repeat(" \t\r\n", 100) + `"abcd"]`, ""},
		"multi-byte counts raw bytes":   {`["éé"]`, ""},
		"multi-byte over":               {`["ééa"]`, tooLongString},
		"number at the limit":           {`[123,456]`, ""},
		"number over":                   {`[1234]`, tooLongValue},
		"signed number over":            {`[-123]`, tooLongValue},
		"float over":                    {`[1e10]`, tooLongValue},
		"literal at the limit":          {`[nul]`, ""},
		"literal over":                  {`[null]`, tooLongValue},
		"number over after a key":       {`{"v":1000}`, tooLongValue},
		"punctuation ends a number":     {`{"a":123,"b":[123],"c":{"d":123}}`, ""},
		"space ends a number":           {`[123 ]`, ""},
		"a string resets the count":     {`[12"x"3]`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			for _, err := range readLimited(t, tt.in) {
				if tt.want == "" && err != nil {
					t.Fatalf("%s refused: %v", tt.in, err)
				}
				if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
					t.Fatalf("%s: error = %v, want %q", tt.in, err, tt.want)
				}
			}
		})
	}
}

func TestTokenLimitReaderStopsAtTheBadByte(t *testing.T) {
	for _, tt := range []struct{ in, before string }{
		{`["abcde"]`, `["abcd`},
		{`[12345]`, `[123`},
	} {
		r := newTokenLimitReader(strings.NewReader(tt.in), 4, 3)
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		if err == nil || string(buf[:n]) != tt.before {
			t.Fatalf("Read(%s) = %q, %v; want %q and an error", tt.in, buf[:n], err, tt.before)
		}
		// The error sticks, so a caller that reads again cannot get past it.
		if n, again := r.Read(buf); n != 0 || again != err {
			t.Fatalf("second Read = %d, %v; want 0, %v", n, again, err)
		}
	}
}

func TestTokenLimitReaderPassesErrors(t *testing.T) {
	boom := errors.New("boom")
	_, err := io.ReadAll(newTokenLimitReader(iotest.ErrReader(boom), 4, 3))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
}
