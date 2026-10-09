package escape

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestName(t *testing.T) {
	for in, want := range map[string]string{
		"notes/USER.md":                  "notes/USER.md",
		"café ⚠ notes.md":                "café ⚠ notes.md",
		"Screenshot 4.27.39\u202fPM.png": "Screenshot 4.27.39\u202fPM.png",
		"no-break\u00a0and\u3000ideo":    "no-break\u00a0and\u3000ideo",
		"real \ufffd stays":              "real \ufffd stays",
		"":                               "",
		"a\x1b[2Jb":                      `"a\x1b[2Jb"`,
		"x\n✓ All 12 files decrypt.md":   `"x\n✓ All 12 files decrypt.md"`,
		"tab\there":                      `"tab\there"`,
		"bad \xff":                       `"bad \xff"`,
		"bidi \u202egpj.exe":             `"bidi \u202egpj.exe"`,
		"joiner\u200d line\u2028sep":     `"joiner\u200d line\u2028sep"`,
		"C1 \u009b2J":                    `"C1 \u009b2J"`,
		"keeps\u202fspaces \x1b":         "\"keeps\u202fspaces \\x1b\"",
	} {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestWriter(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\n":                 "plain text\n",
		"tab\tand é ⚠ …\n":             "tab\tand é ⚠ …\n",
		"Screenshot 4.27.39\u202fPM\n": "Screenshot 4.27.39\u202fPM\n",
		"real \ufffd stays":            "real \ufffd stays",
		"":                             "",
		"\x1b[31mred\x1b[0m":           `\x1b[31mred\x1b[0m`,
		"8-bit CSI \x9b2J":             `8-bit CSI \x9b2J`,
		"C1 CSI \u009b2J":              `C1 CSI \u009b2J`,
		"done\rsalt: ok":               `done\rsalt: ok`,
		"del\x7f":                      `del\x7f`,
		"bidi \u202egpj.exe":           `bidi \u202egpj.exe`,
		"line\u2028sep":                `line\u2028sep`,
		"\xff\xfe start bytes":         `\xff\xfe start bytes`,
	} {
		var out bytes.Buffer
		n, err := Writer(&out).Write([]byte(in))
		if err != nil || n != len(in) {
			t.Errorf("Write(%q) = %d, %v; want %d, nil", in, n, err, len(in))
		}
		if out.String() != want {
			t.Errorf("Write(%q) wrote %q, want %q", in, out.String(), want)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriterError(t *testing.T) {
	for _, in := range []string{"plain", "\x1b"} {
		if n, err := Writer(failWriter{}).Write([]byte(in)); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("Write(%q) = %d, %v; want 0, io.ErrClosedPipe", in, n, err)
		}
	}
}
