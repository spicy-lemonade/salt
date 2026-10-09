// Package escape keeps control characters in text salt did not write, such as
// paths from a backup repo or the output of git and pg_dump, from reaching the
// terminal. Someone who can push chooses those names, and a character such as
// ESC could change what the terminal shows, or a newline could fake a line of
// salt's own output.
package escape

import (
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Name returns s unchanged when it is valid UTF-8 and every character in it
// shows as itself, spaces such as U+202F included, and otherwise quotes it in
// Go syntax, so a name holding a newline or ESC reads as one quoted name.
func Name(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, notGraphic) < 0 {
		return s
	}
	return strconv.QuoteToGraphic(s)
}

func notGraphic(r rune) bool { return !unicode.IsGraphic(r) }

// Writer returns a writer that passes text on to w with every character that
// does not show as itself, other than a newline or a tab, written as a Go
// escape such as \x1b, and every byte that is not UTF-8 as \xNN. Each Write
// must hold whole characters, as one fmt call does.
func Writer(w io.Writer) io.Writer { return writer{w} }

type writer struct{ w io.Writer }

func (e writer) Write(p []byte) (int, error) {
	var out []byte
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRune(p[i:])
		if (r != utf8.RuneError || n > 1) && (unicode.IsGraphic(r) || r == '\n' || r == '\t') {
			if out != nil {
				out = append(out, p[i:i+n]...)
			}
			i += n
			continue
		}
		if out == nil {
			out = append(make([]byte, 0, len(p)+16), p[:i]...)
		}
		q := strconv.QuoteToGraphic(string(p[i : i+n]))
		out = append(out, q[1:len(q)-1]...)
		i += n
	}
	if out == nil {
		out = p
	}
	if _, err := e.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
