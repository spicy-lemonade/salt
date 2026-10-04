package rules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// execAllowed lists the directories (relative to the module root) whose
// non-test code may start processes.
var execAllowed = []string{"internal/gitx", "internal/source", "internal/proc"}

// banned calls: package name -> function -> reason.
var banned = map[string]map[string]string{
	"os": {
		"Executable":   "hooks and cron lines must call salt by name; in tests this is the test binary",
		"StartProcess": "start processes only through os/exec in allowed packages",
	},
	"syscall": {
		"Exec":     "salt must not replace itself",
		"ForkExec": "start processes only through os/exec in allowed packages",
	},
}

func TestProcessSafetyRules(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		checkFile(t, fset, root, rel, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkFile(t *testing.T, fset *token.FileSet, root, rel string, header *ast.File) {
	t.Helper()
	isTest := strings.HasSuffix(rel, "_test.go")
	inE2E := strings.HasPrefix(rel, "test/e2e/")

	if inE2E && !hasBuildTag(header, "e2e") {
		t.Errorf("%s: files in test/e2e must start with //go:build e2e", rel)
	}

	importsExec := false
	for _, imp := range header.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
			importsExec = true
		}
	}
	if importsExec {
		switch {
		case isTest && !inE2E:
			t.Errorf("%s: unit tests must not import os/exec; process tests belong in test/e2e", rel)
		case !isTest && !inE2E && !underAny(rel, execAllowed):
			t.Errorf("%s: only %v may import os/exec", rel, execAllowed)
		}
	}

	full, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
	if err != nil {
		t.Errorf("%s: %v", rel, err)
		return
	}
	ast.Inspect(full, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if reason, ok := banned[pkg.Name][sel.Sel.Name]; ok {
			t.Errorf("%s: %s.%s is banned: %s", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name, reason)
		}
		return true
	})
}

func hasBuildTag(f *ast.File, tag string) bool {
	for _, cg := range f.Comments {
		if cg.Pos() >= f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build ") && strings.Contains(c.Text, tag) {
				return true
			}
		}
	}
	return false
}

func underAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}
