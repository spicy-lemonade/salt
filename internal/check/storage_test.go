package check

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/gitx"
)

func TestBadAttr(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{"text", "unset", false},
		{"text", "set", true},
		{"text", "auto", true},
		{"text", "unspecified", true},
		{"eol", "unspecified", false},
		{"eol", "crlf", true},
		{"filter", "unspecified", false},
		{"filter", "lfs", true},
		{"working-tree-encoding", "unspecified", false},
		{"working-tree-encoding", "UTF-16", true},
		{"ident", "unspecified", false},
		{"ident", "unset", false},
		{"ident", "set", true},
	} {
		if got := badAttr(gitx.Attr{Path: "objects/ab/c.age", Name: tt.name, Value: tt.value}); got != tt.bad {
			t.Errorf("%s=%s: bad = %v, want %v", tt.name, tt.value, got, tt.bad)
		}
	}
}

func TestStorageProblemMessages(t *testing.T) {
	const fix = "; backups could not be restored. Remove the attribute from .gitattributes (or your git config) so *.age stays binary."
	for _, tt := range []struct {
		p    StorageProblem
		want string
	}{
		{StorageProblem{Path: "index.age"},
			"index.age is ignored by git (via .gitignore or your git config), so it would never reach the remote. Remove the matching ignore rule."},
		{StorageProblem{Path: "objects/ab/c.age", Attrs: []BadAttr{{"text", "set"}}},
			"git would change objects/ab/c.age when storing it (text is set)" + fix},
		{StorageProblem{Path: "objects/ab/c.age", Attrs: []BadAttr{{"eol", "crlf"}}},
			"git would change objects/ab/c.age when storing it (eol is set to crlf)" + fix},
		{StorageProblem{Path: "objects/ab/c.age", Attrs: []BadAttr{{"text", "set"}, {"eol", "crlf"}}},
			"git would change objects/ab/c.age when storing it (text is set, eol is set to crlf)" + fix},
		{StorageProblem{Path: "objects/ab/c.age", Attrs: []BadAttr{{"text", "unspecified"}, {"filter", "lfs"}}},
			"git would change objects/ab/c.age when storing it (text is unspecified, filter is set to lfs)" + fix},
		{StorageProblem{Path: "objects/ab/x\n✓ ok.age"},
			`"objects/ab/x\n✓ ok.age" is ignored by git (via .gitignore or your git config), so it would never reach the remote. Remove the matching ignore rule.`},
		{StorageProblem{Path: "objects/ab/\x1b.age", Attrs: []BadAttr{{"eol", "crlf"}}},
			`git would change "objects/ab/\x1b.age" when storing it (eol is set to crlf)` + fix},
		{StorageProblem{Path: "index\r.age", Attrs: []BadAttr{{"text", "unspecified"}}},
			`git may change "index\r.age" when storing it (*.age is not marked binary); backups could not be restored. Add "*.age binary" to .gitattributes.`},
		{StorageProblem{Path: "index.age", Attrs: []BadAttr{{"text", "unspecified"}}},
			`git may change index.age when storing it (*.age is not marked binary); backups could not be restored. Add "*.age binary" to .gitattributes.`},
	} {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("message = %q\nwant      %q", got, tt.want)
		}
	}
}

// git reports every attribute of a path together, so a file with several bad
// attributes is one problem, and the total counts files.
func TestProblemListGroupsByFile(t *testing.T) {
	var l problemList
	l.add(StorageProblem{Path: "objects/ig/nored.age"})
	at := func(path, name, value string) gitx.Attr { return gitx.Attr{Path: path, Name: name, Value: value} }
	for _, a := range []gitx.Attr{
		at("index.age", "text", "unset"), at("index.age", "eol", "unspecified"),
		at("objects/ab/c.age", "text", "set"), at("objects/ab/c.age", "eol", "crlf"), at("objects/ab/c.age", "filter", "unspecified"),
		at("objects/ab/d.age", "text", "unset"), at("objects/ab/d.age", "ident", "set"),
		at("objects/ab/e.age", "text", "unset"),
	} {
		l.attr(a)
	}
	l.flush()
	want := []StorageProblem{
		{Path: "objects/ig/nored.age"},
		{Path: "objects/ab/c.age", Attrs: []BadAttr{{"text", "set"}, {"eol", "crlf"}}},
		{Path: "objects/ab/d.age", Attrs: []BadAttr{{"ident", "set"}}},
	}
	if l.total != 3 || fmt.Sprint(l.problems) != fmt.Sprint(want) {
		t.Fatalf("problems = %v (total %d), want %v (total 3)", l.problems, l.total, want)
	}

	// Past MaxStorageProblems files, only the count grows.
	var many problemList
	for i := 0; i < MaxStorageProblems+5; i++ {
		p := fmt.Sprintf("objects/ab/%d.age", i)
		many.attr(gitx.Attr{Path: p, Name: "text", Value: "set"})
		many.attr(gitx.Attr{Path: p, Name: "eol", Value: "crlf"})
	}
	many.flush()
	if many.total != MaxStorageProblems+5 || len(many.problems) != MaxStorageProblems {
		t.Fatalf("total %d, %d described", many.total, len(many.problems))
	}
}

func TestStorageReport(t *testing.T) {
	ps := []StorageProblem{{Path: "index.age"}, {Path: "objects/ab/c.age", Attrs: []BadAttr{{"text", "set"}}}}
	r := StorageReport("salt check: refusing commit:", ps, 25)
	for _, want := range []string{"salt check: refusing commit: 25 file(s) would not reach the remote intact:\n",
		"  index.age is ignored by git", "  git would change objects/ab/c.age", "  … and 23 more\n"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	if r := StorageReport("x", ps[:1], 1); strings.Contains(r, "more") {
		t.Errorf("report with nothing hidden says more:\n%s", r)
	}
}

// git lists a file salt has just deleted until the deletion is staged. It
// never reaches the remote, so only files on disk are checked.
func TestOnDisk(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "objects", "ab"), 0o755)
	os.WriteFile(filepath.Join(dir, "objects", "ab", "kept.age"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "index.age"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, ".salt-format.json"), []byte("x"), 0o644)
	got, err := onDisk(dir, []string{"index.age", "objects/ab/kept.age", "objects/ab/deleted.age", ".salt-format.json"})
	if err != nil || !slices.Equal(got, []string{"index.age", "objects/ab/kept.age"}) {
		t.Fatalf("onDisk = %v, %v", got, err)
	}
	if _, err := onDisk(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("onDisk on a missing repo succeeded")
	}
}
