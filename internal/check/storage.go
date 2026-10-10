package check

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spicy-lemonade/salt/internal/escape"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// saltPaths are the pathspecs for everything salt writes that must reach the
// remote exactly as written.
var saltPaths = []string{repo.IndexFile, repo.Dir, repo.ObjectsDir, repo.FilesDir}

// storageAttrs are the attributes that make git change a file when it stores
// or checks it out. Ciphertext must have none of them, and text must be unset
// (as `*.age binary` does).
var storageAttrs = []string{"text", "eol", "filter", "working-tree-encoding", "ident"}

// settingsFiles are salt's own settings in the repo, which are text. text and
// eol may change their line ends, which salt reads either way, but no other
// attribute may change them.
var settingsFiles = []string{repo.FormatFile, repo.RecipientsFile}

// MaxStorageProblems is how many files with problems StorageProblems
// describes in full.
const MaxStorageProblems = 20

// StorageProblem is a file salt wrote that git would not store as written:
// it is ignored, or one or more attributes would change it.
type StorageProblem struct {
	Path string
	// Attrs are the attributes that would change the file. None means git
	// ignores it.
	Attrs []BadAttr
}

// BadAttr is an attribute git reports for ciphertext that would change it.
type BadAttr struct{ Name, Value string }

func (a BadAttr) String() string {
	switch a.Value {
	case "set":
		return a.Name + " is set"
	case "unspecified":
		return a.Name + " is unspecified"
	}
	return a.Name + " is set to " + a.Value
}

func (p StorageProblem) String() string {
	name := escape.Name(p.Path)
	switch {
	case len(p.Attrs) == 0:
		return fmt.Sprintf("%s is ignored by git (via .gitignore or your git config), so it would never reach the remote. Remove the matching ignore rule.", name)
	case len(p.Attrs) == 1 && p.Attrs[0] == BadAttr{"text", "unspecified"}:
		return fmt.Sprintf("git may change %s when storing it (*.age is not marked binary); backups could not be restored. Add \"*.age binary\" to .gitattributes.", name)
	}
	descs := make([]string, len(p.Attrs))
	for i, a := range p.Attrs {
		descs[i] = a.String()
	}
	if slices.Contains(settingsFiles, p.Path) {
		return fmt.Sprintf("git would change %s when storing it or checking it out (%s), so salt could not read it from another copy of the repo. Remove the attribute from .gitattributes (or your git config).",
			name, strings.Join(descs, ", "))
	}
	return fmt.Sprintf("git would change %s when storing it (%s); backups could not be restored. Remove the attribute from .gitattributes (or your git config) so *.age stays binary.",
		name, strings.Join(descs, ", "))
}

// badAttr reports whether an attribute would make git change ciphertext, or
// one of settingsFiles other than in its line ends.
func badAttr(a gitx.Attr) bool {
	if slices.Contains(settingsFiles, a.Path) && (a.Name == "text" || a.Name == "eol") {
		return false
	}
	switch a.Name {
	case "text":
		return a.Value != "unset"
	case "ident":
		return a.Value != "unspecified" && a.Value != "unset"
	}
	return a.Value != "unspecified"
}

// problemList collects problems one file at a time, keeping the first
// MaxStorageProblems and counting every file.
type problemList struct {
	problems []StorageProblem
	total    int
	cur      *StorageProblem
}

func (l *problemList) add(p StorageProblem) {
	l.total++
	if len(l.problems) < MaxStorageProblems {
		l.problems = append(l.problems, p)
	}
}

// attr takes check-attr's output one attribute at a time. git reports all of
// a path's attributes together, so a file's bad attributes are gathered into
// one problem.
func (l *problemList) attr(a gitx.Attr) {
	if !badAttr(a) {
		return
	}
	if l.cur != nil && l.cur.Path != a.Path {
		l.flush()
	}
	if l.cur == nil {
		l.cur = &StorageProblem{Path: a.Path}
	}
	l.cur.Attrs = append(l.cur.Attrs, BadAttr{a.Name, a.Value})
}

func (l *problemList) flush() {
	if l.cur != nil {
		l.add(*l.cur)
		l.cur = nil
	}
}

// StorageProblems asks git whether it would store every file salt wrote in
// the repository at dir exactly as written. It reports files git ignores,
// and ciphertext and settingsFiles with attributes that would change them,
// whether the file is tracked already or not yet added. At most MaxStorageProblems files are
// described; total counts them all.
func StorageProblems(dir string) (problems []StorageProblem, total int, err error) {
	var l problemList
	ignored, err := gitx.IgnoredPaths(dir, saltPaths...)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range ignored {
		l.add(StorageProblem{Path: p})
	}
	paths, err := gitx.AddablePaths(dir, saltPaths...)
	if err != nil {
		return nil, 0, err
	}
	written, err := onDisk(dir, paths)
	if err != nil {
		return nil, 0, err
	}
	err = gitx.CheckAttrs(dir, written, storageAttrs, func(a gitx.Attr) error {
		l.attr(a)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	l.flush()
	return l.problems, l.total, nil
}

// onDisk keeps the ciphertext paths and settingsFiles that exist in the
// working tree. git still lists a file salt has just deleted until the
// deletion is staged, and that file never reaches the remote, so it is not
// a problem.
func onDisk(dir string, paths []string) ([]string, error) {
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	var out []string
	for _, p := range paths {
		if !strings.HasSuffix(p, ".age") && !slices.Contains(settingsFiles, p) {
			continue
		}
		if _, err := rt.Lstat(filepath.FromSlash(p)); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// StorageReport formats storage problems for the terminal, after a first line
// saying what they mean for the command that found them.
func StorageReport(lead string, problems []StorageProblem, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d file(s) would not reach the remote intact:\n", lead, total)
	for _, p := range problems {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	if total > len(problems) {
		fmt.Fprintf(&b, "  … and %d more\n", total-len(problems))
	}
	return b.String()
}
