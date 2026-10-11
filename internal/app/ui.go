package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

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
	// Context, if set, stops ReadLine waiting for an answer as soon as it
	// is cancelled, as by Ctrl-C in a command that catches it, which would
	// otherwise notice only once the question was answered. A question asked
	// once it is cancelled, by ReadLine or ReadSecret, returns its error at
	// once, so nothing reads the terminal after. ReadSecret needs nothing
	// more, as Ctrl-C reaches it as a key (see readHidden).
	Context context.Context
}

// isTerminal reports whether fd is a terminal. Tests replace it, so they
// behave the same whether or not they run in a terminal. makeRaw and
// termRestore are term's own, which tests replace so that a secret is read
// without a terminal.
var (
	isTerminal  = term.IsTerminal
	makeRaw     = term.MakeRaw
	termRestore = term.Restore
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
	})
}

func (t *Terminal) ReadSecret(prompt string) (string, error) {
	if t.Context != nil && t.Context.Err() != nil {
		return "", t.Context.Err()
	}
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return t.ReadLine(prompt)
	}
	fmt.Fprint(t.out, prompt)
	s, err := t.readHidden(fd)
	fmt.Fprintln(t.out)
	return s, err
}

// Keys readHidden acts on.
const (
	ctrlC     = 3
	ctrlD     = 4
	backspace = 8
	ctrlU     = 21
	del       = 127
)

// readHidden reads a line from the terminal fd without showing it. The
// terminal is in raw mode meanwhile, so Ctrl-C arrives as a key rather than
// a signal, and stops the question at once with ErrInterrupted, whether or
// not the command catches signals, and the terminal is always put back as
// it was. term.ReadPassword leaves signals on, so Ctrl-C at it ends salt
// with the terminal still hiding what is typed, or is caught and ignored.
// Backspace removes the last character, Ctrl-U the whole line, and Ctrl-D
// on an empty line ends the input. Every other byte is kept, a tab or any
// other control character included, as term.ReadPassword keeps them, so a
// passphrase set with one, as through --passphrase-file, can be typed. A
// line ends at \r or \n, and a \n already read in straight after a \r, as
// when a line copied on Windows is pasted, ends it too, so the next question
// does not take it for an empty answer.
func (t *Terminal) readHidden(fd int) (string, error) {
	state, err := makeRaw(fd)
	if err != nil {
		return "", err
	}
	defer termRestore(fd, state)
	var line []byte
	for {
		c, err := t.in.ReadByte()
		switch {
		case errors.Is(err, io.EOF) && len(line) > 0:
			return string(line), nil
		case err != nil:
			return "", err
		}
		switch c {
		case '\r':
			// Only what has already arrived is looked at, so this never
			// waits for another key.
			if t.in.Buffered() > 0 {
				if next, _ := t.in.Peek(1); next[0] == '\n' {
					t.in.Discard(1)
				}
			}
			return string(line), nil
		case '\n':
			return string(line), nil
		case ctrlC:
			return "", ErrInterrupted
		case ctrlD:
			if len(line) == 0 {
				return "", io.EOF
			}
		case backspace, del:
			_, n := utf8.DecodeLastRune(line)
			line = line[:len(line)-n]
		case ctrlU:
			line = line[:0]
		default:
			line = append(line, c)
		}
	}
}

// wait returns what read, which waits for the person, returns, or Context's
// error as soon as it is cancelled. read is then left waiting, as the
// command is stopping, and Context's error is returned from then on, so
// nothing else reads from the terminal.
func (t *Terminal) wait(read func() (string, error)) (string, error) {
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
