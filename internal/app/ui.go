package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// UI is how commands talk to the person. Everything goes to stderr, so stdout
// stays empty for cron jobs that forward any stdout as a message.
type UI interface {
	Printf(format string, a ...any)
	ReadLine(prompt string) (string, error)
	ReadSecret(prompt string) (string, error)
	// Interactive reports whether a person is at the terminal.
	Interactive() bool
	// Clear hides what was printed (the recovery phrase) from the screen.
	Clear()
}

// ErrNotInteractive is returned when a command needs a person at a terminal.
var ErrNotInteractive = errors.New("this needs an interactive terminal")

// Terminal is the real UI on stdin/stderr.
type Terminal struct {
	in  *bufio.Reader
	out io.Writer
}

func NewTerminal() *Terminal {
	return &Terminal{in: bufio.NewReader(os.Stdin), out: EscapeWriter(os.Stderr)}
}

// EscapeWriter returns a writer that passes text on to w with every character
// that is not printable, other than a newline or a tab, written as a Go
// escape such as \x1b, and every byte that is not UTF-8 as \xNN. Messages
// can hold paths from a backup repo and the output of git or pg_dump, which
// someone who can push may choose, and a control character among them could
// change what the terminal shows. Each Write must hold whole characters, as
// one fmt call does.
func EscapeWriter(w io.Writer) io.Writer { return escapeWriter{w} }

type escapeWriter struct{ w io.Writer }

func (e escapeWriter) Write(p []byte) (int, error) {
	var out []byte
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRune(p[i:])
		if (r != utf8.RuneError || n > 1) && (unicode.IsPrint(r) || r == '\n' || r == '\t') {
			if out != nil {
				out = append(out, p[i:i+n]...)
			}
			i += n
			continue
		}
		if out == nil {
			out = append(make([]byte, 0, len(p)+16), p[:i]...)
		}
		q := strconv.Quote(string(p[i : i+n]))
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

func (t *Terminal) Printf(format string, a ...any) { fmt.Fprintf(t.out, format, a...) }

func (t *Terminal) ReadLine(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	s, err := t.in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func (t *Terminal) ReadSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return t.ReadLine(prompt)
	}
	fmt.Fprint(t.out, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(t.out)
	return string(b), err
}

func (t *Terminal) Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func (t *Terminal) Clear() {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		// Clear screen and scrollback, cursor home. These codes are salt's
		// own, so they go straight to stderr rather than through t.out,
		// which would escape them.
		fmt.Fprint(os.Stderr, "\033[H\033[2J\033[3J")
	}
}
