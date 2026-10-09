package gitx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// NewBlobs lists every file that the commits reachable from tips, and not
// from have or from remote's remote-tracking branches, add or change, as a
// push of tips would send them. Each blob is listed once for each path it
// is at. A merge is compared with each of its parents, and a first commit
// with nothing. have may name commits this repository does not hold, such
// as a branch tip on the remote that prune has dropped here, which are left
// out. remote may be "" for none. tips and have must be object IDs. They
// are given to git on its input, so their number is not limited.
func NewBlobs(dir string, tips, have []string, remote string) ([]Blob, error) {
	args := []string{"log", "--stdin", "--ignore-missing", "-z", "--raw", "--no-abbrev", "--no-renames",
		"--format=", "-m", "--root", "--diff-filter=ACMRT"}
	if remote != "" {
		args = append(args, "--remotes="+remote)
	}
	cmd := exec.Command("git", Args(dir, append(args, "--")...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		w := bufio.NewWriter(stdin)
		// Older git reads no options, such as --not, on its input.
		for _, id := range tips {
			fmt.Fprintln(w, id)
		}
		for _, id := range have {
			fmt.Fprintln(w, "^"+id)
		}
		w.Flush()
		stdin.Close()
	}()
	blobs, err := readRaw(stdout)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return blobs, nil
}

// readRaw reads `git log -z --raw --format=` output: for each file changed,
// ":MODE MODE ID ID STATUS" and then its path, each ending in NUL, with a
// newline before the first of each commit. It returns each new blob ID
// with its path, each pair once.
func readRaw(r io.Reader) ([]Blob, error) {
	br := bufio.NewReader(r)
	seen := map[Blob]bool{}
	var out []Blob
	for {
		tok, err := br.ReadString(0)
		if errors.Is(err, io.EOF) && strings.TrimSpace(tok) == "" {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("git log: %w", err)
		}
		tok = strings.TrimLeft(strings.TrimSuffix(tok, "\x00"), "\n")
		if tok == "" {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(tok, ":"))
		if !strings.HasPrefix(tok, ":") || len(fields) != 5 {
			return nil, fmt.Errorf("git log: unexpected output %.80q", tok)
		}
		path, err := br.ReadString(0)
		if err != nil {
			return nil, fmt.Errorf("git log: no path after %.80q", tok)
		}
		b := Blob{ID: fields[3], Path: strings.TrimSuffix(path, "\x00")}
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
}
