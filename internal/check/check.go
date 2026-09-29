// Package check is the pre-commit guard: it refuses a commit that stages any
// file which is not age ciphertext, apart from the repo's public files.
package check

import (
	"bytes"
	"fmt"
	"strings"

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

func (v Violation) String() string { return v.Path + ": " + v.Reason }

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
	var out []Violation
	err = gitx.StagedHeads(dir, paths, HeadSize, func(p string, head []byte, ok bool) error {
		if v := Classify(p, head, ok); v != nil {
			out = append(out, *v)
		}
		return nil
	})
	return out, err
}

// Report formats violations for the terminal.
func Report(vs []Violation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "salt check: refusing commit: %d staged file(s) are not encrypted:\n", len(vs))
	for i, v := range vs {
		if i == 20 {
			fmt.Fprintf(&b, "  … and %d more\n", len(vs)-20)
			break
		}
		fmt.Fprintf(&b, "  %s\n", v)
	}
	b.WriteString("Unstage them (git restore --staged <path>) and encrypt with `salt seal` instead.\n")
	return b.String()
}
