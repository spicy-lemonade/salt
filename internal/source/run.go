// Package source makes safe copies of live databases for salt to seal. It is
// the only package besides gitx that starts other programs.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// maxStderr caps how much of a program's error output is kept.
const maxStderr = 4 << 10

// ErrMissingProgram means a program salt needs could not be found.
var ErrMissingProgram = errors.New("is not installed or not on PATH")

// run runs cmd, made with exec.CommandContext(ctx, ...). Its error output is
// kept, up to maxStderr, to explain a failure. When ctx is cancelled, the
// program is stopped and ctx's error is returned.
func run(ctx context.Context, cmd *exec.Cmd) error {
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderr
	err := cmd.Run()
	name := cmd.Args[0]
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("salt needs the %s program, which %w", name, ErrMissingProgram)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
// The buffer is a field, not embedded, so io.Copy cannot reach
// bytes.Buffer.ReadFrom and skip the limit.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
