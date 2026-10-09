package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spicy-lemonade/salt/internal/escape"
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
	in *bufio.Reader
	// out escapes control characters, since messages can quote names from
	// the backup repo; raw is the same stream unescaped, for Clear's codes.
	out, raw io.Writer
}

func NewTerminal() *Terminal {
	return &Terminal{in: bufio.NewReader(os.Stdin), out: escape.Writer(os.Stderr), raw: os.Stderr}
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
		// Clear screen and scrollback, cursor home.
		fmt.Fprint(t.raw, "\033[H\033[2J\033[3J")
	}
}
