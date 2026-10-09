// Package check is the pre-commit and pre-push guard: it refuses a commit
// that stages, or a push that sends, any file which is not age ciphertext,
// apart from the repo's public files.
package check

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/spicy-lemonade/salt/internal/escape"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/repo"
)

var (
	binaryHeader = []byte("age-encryption.org/v1\n")
	armorHeader  = []byte("-----BEGIN AGE ENCRYPTED FILE-----")
)

// HeadSize is how many bytes of each staged file are inspected.
const HeadSize = len("-----BEGIN AGE ENCRYPTED FILE-----")

// Violation is a staged file that must not be committed.
type Violation struct {
	Path   string
	Reason string
}

func (v Violation) String() string { return escape.Name(v.Path) + ": " + v.Reason }

// Classify decides whether a staged file may be committed, given its first
// HeadSize bytes. ok is false when the blob could not be read as a file.
func Classify(path string, head []byte, ok bool) *Violation {
	if repo.Public[path] {
		return nil
	}
	if !ok {
		return &Violation{path, "not a regular file"}
	}
	if bytes.HasPrefix(head, binaryHeader) || bytes.HasPrefix(head, armorHeader) {
		return nil
	}
	return &Violation{path, "not encrypted"}
}

// Staged checks every staged file in the repository at dir.
func Staged(dir string) ([]Violation, error) {
	paths, err := gitx.StagedPaths(dir)
	if err != nil {
		return nil, err
	}
	return classifyBlobs(dir, "", paths)
}

// Committed checks every file in the last commit of the repository at dir.
func Committed(dir string) ([]Violation, error) {
	paths, err := gitx.TreePaths(dir, "HEAD")
	if err != nil {
		return nil, err
	}
	return classifyBlobs(dir, "HEAD", paths)
}

// Pushed checks every file that the commits a push sends add or change: those
// reachable from tips and not from have or remote's remote-tracking branches
// (see gitx.NewBlobs). A file merging or rebasing brought in, or that was
// committed with --no-verify, is checked too.
func Pushed(dir string, tips, have []string, remote string) ([]Violation, error) {
	blobs, err := gitx.NewBlobs(dir, tips, have, remote)
	if err != nil {
		return nil, err
	}
	var out []Violation
	err = gitx.BlobIDHeads(dir, blobs, HeadSize, collect(&out))
	return out, err
}

func classifyBlobs(dir, rev string, paths []string) ([]Violation, error) {
	var out []Violation
	err := gitx.BlobHeads(dir, rev, paths, HeadSize, collect(&out))
	return out, err
}

// collect returns a function that classifies each blob it is given and adds
// those that must not be committed to out.
func collect(out *[]Violation) func(p string, head []byte, ok bool) error {
	return func(p string, head []byte, ok bool) error {
		if v := Classify(p, head, ok); v != nil {
			*out = append(*out, *v)
		}
		return nil
	}
}

// Report formats violations in a commit for the terminal.
func Report(vs []Violation) string {
	return report(vs, "salt check: refusing commit: %d staged file(s) are not encrypted:\n",
		"Unstage them (git restore --staged <path>) and encrypt with `salt seal` instead.\n")
}

// PushReport formats violations in a push for the terminal.
func PushReport(vs []Violation) string {
	return report(vs, "salt check: refusing push: %d file(s) in the commits being pushed are not encrypted:\n",
		"Remove them from those commits, for example with git rebase, and encrypt with `salt seal` instead.\n")
}

// report lists the first 20 violations between lead, given how many there
// are, and advice.
func report(vs []Violation, lead, advice string) string {
	var b strings.Builder
	fmt.Fprintf(&b, lead, len(vs))
	for i, v := range vs {
		if i == 20 {
			fmt.Fprintf(&b, "  … and %d more\n", len(vs)-20)
			break
		}
		fmt.Fprintf(&b, "  %s\n", v)
	}
	b.WriteString(advice)
	return b.String()
}
