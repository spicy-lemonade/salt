// Package proc runs the programs that gitx and source start, so each one is
// stopped the same way and none can hold salt up or fill its memory. Only
// gitx, source and proc may use os/exec (see internal/rules).
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// MaxStderr caps how much of a program's error output is kept.
const MaxStderr = 4 << 10

// WaitDelay bounds how long a stopped program's output is waited for, in case
// a program it started still holds it open.
const WaitDelay = 5 * time.Second

// ErrMissingProgram means a program salt needs could not be found.
var ErrMissingProgram = errors.New("is not installed or not on PATH")

// Error is a program that failed, with its error output to explain why.
type Error struct {
	// Program names the program in the message, such as "pg_dump".
	Program string
	Err     error
	// Stderr is the program's error output, cut back to whole lines when it
	// was longer than MaxStderr.
	Stderr string
}

func (e *Error) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s: %v", e.Program, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Program, e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Run runs cmd, made with exec.CommandContext(ctx, ...). Its error output is
// kept, up to MaxStderr, to explain a failure in an *Error. When ctx is
// cancelled, the program is stopped and ctx's error is returned. A program
// stopped by SIGINT or SIGTERM returns context.Canceled too, since Ctrl-C
// reaches every program in the terminal's process group and can stop the
// program before salt has cancelled ctx.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	stderr := &LimitedBuffer{Max: MaxStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = WaitDelay
	err := cmd.Run()
	name := cmd.Args[0]
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("salt needs the %s program, which %w", name, ErrMissingProgram)
	case ctx.Err() != nil:
		return ctx.Err()
	case stoppedBySignal(err):
		return context.Canceled
	}
	return &Error{Program: name, Err: err, Stderr: stderr.Lines()}
}

// stoppedBySignal reports whether err is from a program that SIGINT or
// SIGTERM stopped.
func stoppedBySignal(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && (ws.Signal() == syscall.SIGINT || ws.Signal() == syscall.SIGTERM)
}

// LimitedBuffer keeps the first Max bytes written to it and drops the rest.
// The buffer is a field, not embedded, so io.Copy cannot reach
// bytes.Buffer.ReadFrom and skip the limit.
type LimitedBuffer struct {
	Max int
	buf bytes.Buffer
	cut bool
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	room := max(b.Max-b.buf.Len(), 0)
	b.buf.Write(p[:min(len(p), room)])
	b.cut = b.cut || len(p) > room
	return len(p), nil
}

func (b *LimitedBuffer) String() string { return b.buf.String() }

// Lines returns what was kept, trimmed of surrounding space. When the rest
// was dropped, the last line, which was cut part way, is left out, so a
// secret the caller hides by searching for it whole cannot show in part.
func (b *LimitedBuffer) Lines() string {
	s := b.buf.String()
	if b.cut {
		end := strings.LastIndexByte(s, '\n')
		s = s[:max(end, 0)]
	}
	return strings.TrimSpace(s)
}
