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
	Abs  string // where it is
	Rel  string // the slash path it is backed up under
	Real string // Abs with its symlinks followed
}

// varName matches an environment variable's name.
const varName = `[A-Za-z_][A-Za-z0-9_]*`

// variable matches ${VAR} and ${VAR:-DEFAULT}.
var variable = regexp.MustCompile(`\$\{(` + varName + `)(:-([^}]*))?\}`)

// expand returns template with its variables and leading ~ filled in. ok is
// false when a variable without a default is unset or empty, or ~ is used
// without a home folder, and the path is then skipped.
func (e Env) expand(template string) (string, bool) {
	ok := true
	out := variable.ReplaceAllStringFunc(template, func(m string) string {
		sub := variable.FindStringSubmatch(m)
		v := e.getenv(sub[1])
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

// vars lists the variables template reads, each once. set holds those set
// to something and defaulted those unset with a default wherever they are
// read, each in order of first use. missing holds those unset that are read
// at least once without a default, in order of their first such read. An
// empty variable counts as unset, as in expand.
func (e Env) vars(template string) (set, defaulted, missing []string) {
	for _, m := range variable.FindAllStringSubmatch(template, -1) {
		switch name := m[1]; {
		case slices.Contains(set, name) || slices.Contains(missing, name):
		case e.getenv(name) != "":
			set = append(set, name)
		case m[2] == "":
			defaulted = slices.DeleteFunc(defaulted, func(n string) bool { return n == name })
			missing = append(missing, name)
		case !slices.Contains(defaulted, name):
			defaulted = append(defaulted, name)
		}
	}
	return set, defaulted, missing
}

// getenv returns the environment variable name, or "" when it is unset or
// e has no Getenv.
func (e Env) getenv(name string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(name)
}

// Find returns every place on this machine that the preset path x names and that
// exists. Its parts holding a * are matched against folders, never against
// what a variable holds, so a path holding * is taken as it is. Following a
// place's symlinks tells both that it exists and where it really is, in one
// step, so it cannot be deleted in between.
func (e Env) Find(x Path) ([]Place, error) {
	runs, pats := splitStars(x.From)
	first, ok := e.expand(runs[0])
	if !ok {
		return nil, nil
	}
	abs, err := filepath.Abs(filepath.FromSlash(first))
	if err != nil {
		return nil, err
	}
	// Each found place keeps the text each of its * matched.
	type match struct {
		abs   string
		stars []string
	}
	places := []match{{abs: abs}}
	for i, rest := range runs[1:] {
		tail, ok := e.expand(rest)
		if !ok && rest != "" {
			return nil, nil
		}
		var next []match
		for _, pl := range places {
			found, err := folders(pl.abs, pats[i])
			if err != nil {
				return nil, err
			}
			for _, f := range found {
				next = append(next, match{
					abs:   filepath.Join(pl.abs, f.name, filepath.FromSlash(tail)),
					stars: append(slices.Clip(pl.stars), f.star),
				})
			}
		}
		places = next
	}
	var found []Place
	for _, pl := range places {
		real, err := filepath.EvalSymlinks(pl.abs)
		switch {
		case err == nil:
			found = append(found, Place{Abs: pl.abs, Rel: fill(x.To, pl.stars), Real: real})
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	return found, nil
}

// splitStars splits the template from at each part holding a *, into the
// fixed runs between them and those parts.
func splitStars(from string) (runs, pats []string) {
	run := []string{}
	for _, part := range strings.Split(from, "/") {
		if strings.Contains(part, "*") {
			runs = append(runs, strings.Join(run, "/"))
			pats = append(pats, part)
			run = []string{}
			continue
		}
		run = append(run, part)
	}
	return append(runs, strings.Join(run, "/")), pats
}

// checkFrom refuses a template whose variables expand cannot read: one
// nested in another's default, a $ that starts no variable, a * inside a
// variable or in the same part as one, or a * in the first part, which
// would match from the current folder.
func checkFrom(from string) error {
	runs, pats := splitStars(from)
	if runs[0] == "" {
		return fmt.Errorf("%s: its first part cannot hold a *", from)
	}
	for _, pat := range pats {
		if strings.ContainsAny(pat, "${}") {
			return fmt.Errorf("%s: a part holding * cannot hold a variable", from)
		}
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

// fill returns the slash path to with the * in each part holding one
// replaced by the next of stars.
func fill(to string, stars []string) string {
	parts := strings.Split(to, "/")
	for i, part := range parts {
		if strings.Contains(part, "*") {
			parts[i], stars = strings.Replace(part, "*", stars[0], 1), stars[1:]
		}
	}
	return strings.Join(parts, "/")
}

// matchPart returns the text the * in the path part pat matches in name,
// and whether pat holds a * and matches name. The * matches at least one
// character, and never a whole . or .., so it can fill a part on its own.
func matchPart(pat, name string) (star string, ok bool) {
	before, after, found := strings.Cut(pat, "*")
	if !found || len(name) <= len(before)+len(after) || !strings.HasPrefix(name, before) || !strings.HasSuffix(name, after) {
		return "", false
	}
	if star = name[len(before) : len(name)-len(after)]; star == "." || star == ".." {
		return "", false
	}
	return star, true
}

// folder is a folder a part holding a * matched, and the text the * matched.
type folder struct{ name, star string }

// folders lists the folders in dir that the part pat, which holds a *,
// matches. Hidden folders are left out, as a shell's * leaves them out,
// unless pat starts with a dot. A missing dir has none.
func folders(dir, pat string) ([]folder, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hidden := strings.HasPrefix(pat, ".")
	var found []folder
	for _, d := range entries {
		star, ok := matchPart(pat, d.Name())
		if !ok || (strings.HasPrefix(d.Name(), ".") && !hidden) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, d.Name())); err == nil && fi.IsDir() {
			found = append(found, folder{name: d.Name(), star: star})
		}
	}
	return found, nil
}
