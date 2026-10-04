package preset

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Env is what a preset's paths are read with. Tests give their own, so they
// never read the real home folder or environment.
type Env struct {
	// Getenv returns an environment variable, or "" when it is unset.
	Getenv func(string) string
	// Home is the home folder, or "" when there is none; a path starting
	// with ~ is then skipped.
	Home string
}

// Place is a file or folder a preset path found on this machine.
type Place struct {
	Abs string // where it is
	Rel string // the slash path it is backed up under
}

// variable matches ${VAR} and ${VAR:-DEFAULT}.
var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expand returns template with its variables and leading ~ filled in. ok is
// false when a variable without a default is unset or empty, or ~ is used
// without a home folder, and the path is then skipped.
func (e Env) expand(template string) (string, bool) {
	ok := true
	out := variable.ReplaceAllStringFunc(template, func(m string) string {
		sub := variable.FindStringSubmatch(m)
		v := ""
		if e.Getenv != nil {
			v = e.Getenv(sub[1])
		}
		switch {
		case v != "":
			return v
		case sub[2] != "":
			return sub[3]
		}
		ok = false
		return ""
	})
	if out == "~" || strings.HasPrefix(out, "~/") {
		if e.Home == "" {
			return "", false
		}
		out = e.Home + out[1:]
	}
	return out, ok && out != ""
}

// Find returns every place on this machine that the preset path x names and that
// exists. Its parts that are only * are matched against folders, never
// against what a variable holds, so a path holding * is taken as it is.
func (e Env) Find(x Path) ([]Place, error) {
	runs := splitStars(x.From)
	first, ok := e.expand(runs[0])
	if !ok {
		return nil, nil
	}
	abs, err := filepath.Abs(filepath.FromSlash(first))
	if err != nil {
		return nil, err
	}
	// Each found place keeps the folder names its * parts matched.
	type match struct {
		abs   string
		names []string
	}
	places := []match{{abs: abs}}
	for _, rest := range runs[1:] {
		tail, ok := e.expand(rest)
		if !ok && rest != "" {
			return nil, nil
		}
		var next []match
		for _, pl := range places {
			names, err := folders(pl.abs)
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				next = append(next, match{
					abs:   filepath.Join(pl.abs, n, filepath.FromSlash(tail)),
					names: append(slices.Clip(pl.names), n),
				})
			}
		}
		places = next
	}
	var found []Place
	for _, pl := range places {
		_, err := os.Stat(pl.abs)
		switch {
		case err == nil:
			found = append(found, Place{Abs: pl.abs, Rel: fill(x.To, pl.names)})
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	return found, nil
}

// splitStars splits the template from at each part that is only *, into
// the fixed runs between them.
func splitStars(from string) []string {
	var runs []string
	run := []string{}
	for _, part := range strings.Split(from, "/") {
		if part == "*" {
			runs = append(runs, strings.Join(run, "/"))
			run = []string{}
			continue
		}
		run = append(run, part)
	}
	return append(runs, strings.Join(run, "/"))
}

// checkFrom refuses a template whose variables expand cannot read: one
// nested in another's default, a $ that starts no variable, a * inside a
// variable, or a * as the first part, which would match from the current
// folder.
func checkFrom(from string) error {
	runs := splitStars(from)
	if runs[0] == "" {
		return fmt.Errorf("%s cannot start with *", from)
	}
	for _, run := range runs {
		bad := strings.ContainsAny(variable.ReplaceAllString(run, ""), "${}")
		for _, m := range variable.FindAllStringSubmatch(run, -1) {
			bad = bad || strings.ContainsAny(m[3], "${")
		}
		if bad {
			return fmt.Errorf("%s: a variable must be ${NAME} or ${NAME:-DEFAULT}, with no $, { or } in DEFAULT and no * in either", from)
		}
	}
	return nil
}

// fill returns the slash path to with each part that is only * replaced by
// the next of names.
func fill(to string, names []string) string {
	parts := strings.Split(to, "/")
	for i, part := range parts {
		if part == "*" {
			parts[i], names = names[0], names[1:]
		}
	}
	return strings.Join(parts, "/")
}

// folders lists the names of the folders in dir, leaving out hidden ones as
// a shell's * does. A missing dir has none.
func folders(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, d.Name())); err == nil && fi.IsDir() {
			names = append(names, d.Name())
		}
	}
	return names, nil
}
