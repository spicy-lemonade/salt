package app

import (
	"bufio"
	"context"
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
	// Context, if set, stops a question waiting for an answer as soon as it
	// is cancelled, as by Ctrl-C in a command that catches it, which would
	// otherwise notice only once the question was answered. A question
	// asked once it is cancelled returns its error at once.
	Context context.Context
}

// isTerminal reports whether fd is a terminal. Tests replace it, so they
// behave the same whether or not they run in a terminal. readPassword,
// termState and termRestore are term's own, which tests replace so that a
// secret is read without a terminal.
var (
	isTerminal   = term.IsTerminal
	readPassword = term.ReadPassword
	termState    = term.GetState
	termRestore  = term.Restore
)

func NewTerminal() *Terminal {
	return &Terminal{in: bufio.NewReader(os.Stdin), out: escape.Writer(os.Stderr), raw: os.Stderr}
}

func (t *Terminal) Printf(format string, a ...any) { fmt.Fprintf(t.out, format, a...) }

func (t *Terminal) ReadLine(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	return t.wait(func() (string, error) {
		s, err := t.in.ReadString('\n')
		if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
			return "", err
		}
		return strings.TrimRight(s, "\r\n"), nil
	}, nil)
}

func (t *Terminal) ReadSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return t.ReadLine(prompt)
	}
	// The terminal stops showing what is typed while the secret is read. If
	// the question is stopped, it is told to show it again, since the read
	// itself is left waiting and never puts it back.
	state, err := termState(fd)
	if err != nil {
		return "", err
	}
	fmt.Fprint(t.out, prompt)
	read := readPassword
	s, err := t.wait(func() (string, error) {
		b, err := read(fd)
		return string(b), err
	}, func() { termRestore(fd, state) })
	fmt.Fprintln(t.out)
	return s, err
}

// wait returns what read, which waits for the person, returns, or Context's
// error as soon as it is cancelled, after calling stopped, if set. read is
// then left waiting, as the command is stopping, and Context's error is
// returned from then on, so nothing else reads from the terminal.
func (t *Terminal) wait(read func() (string, error), stopped func()) (string, error) {
	if t.Context == nil {
		return read()
	}
	if err := t.Context.Err(); err != nil {
		return "", err
	}
	type answer struct {
		s   string
		err error
	}
	got := make(chan answer, 1)
	go func() {
		s, err := read()
		got <- answer{s, err}
	}()
	select {
	case a := <-got:
		return a.s, a.err
	case <-t.Context.Done():
		if stopped != nil {
			stopped()
		}
		return "", t.Context.Err()
	}
}

func (t *Terminal) Interactive() bool {
	return isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stderr.Fd()))
}

func (t *Terminal) Clear() {
	if isTerminal(int(os.Stderr.Fd())) {
		// Clear screen and scrollback, cursor home.
		fmt.Fprint(t.raw, "\033[H\033[2J\033[3J")
	}
}
