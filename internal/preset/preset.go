// Package preset knows where tools keep the files needed to restore their
// memory, so salt backup can gather them without a script. Each preset is a
// small JSON file in presets/, read by the same code, so adding a tool means
// adding one file and no Go.
package preset

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/spicy-lemonade/salt/internal/repo"
)

//go:embed presets/*.json
var builtin embed.FS

// Preset says where one tool keeps the files needed to restore its memory.
type Preset struct {
	// Name is how the preset is chosen, as in salt backup --preset NAME. It is
	// the file's name without .json.
	Name string `json:"name"`
	// About describes the preset in one line.
	About string `json:"about"`
	// Paths lists each file or folder to back up.
	Paths []Path `json:"paths"`
	// Skip lists name patterns (path.Match) of files and folders never backed
	// up, such as caches.
	Skip []string `json:"skip"`
	// Secrets lists files that may hold secrets, which are left out when they
	// do.
	Secrets []Secret `json:"secrets"`
}

// Path is one file or folder to back up.
type Path struct {
	// From is where it is on this machine. ${VAR} is the environment
	// variable VAR, and the path is skipped when VAR is unset or empty.
	// ${VAR:-DEFAULT} uses DEFAULT then. A leading ~ is the home folder. A
	// part that is only * matches every folder there, such as each profile.
	From string `json:"from"`
	// To is the slash path it is backed up under. It has a * for each * in
	// From, which takes the name that * matched.
	To string `json:"to"`
}

// Secret names files that may hold secrets, such as API keys, and the
// settings in them that do. A file is backed up only when every such setting
// is empty.
type Secret struct {
	// Files lists name patterns (path.Match) of the files to check. They are
	// read as YAML, which includes JSON.
	Files []string `json:"files"`
	// Keys lists name patterns (path.Match) of settings that hold secrets,
	// compared in lower case at any depth in the file, beyond those in
	// DefaultSecretKeys, which are always checked.
	Keys []string `json:"keys"`
}

// DefaultSecretKeys lists name patterns (path.Match) of settings that hold
// secrets in most tools' settings files. They are checked in every file a
// secrets rule names, with that rule's own keys.
var DefaultSecretKeys = []string{
	"*api_key", "*apikey", "*api-key", "*secret", "*secret_key", "*access_key", "*private_key",
	"*password", "*passphrase", "token", "*_token", "*-token", "authorization",
}

// ErrUnknown means no preset has the name asked for.
var ErrUnknown = errors.New("unknown preset")

// Names lists the built-in presets' names in order.
func Names() []string {
	entries, _ := fs.ReadDir(builtin, "presets")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names
}

// Get returns the built-in preset called name.
func Get(name string) (*Preset, error) {
	if !slices.Contains(Names(), name) {
		return nil, fmt.Errorf("%w %q; the presets are %s", ErrUnknown, name, strings.Join(Names(), ", "))
	}
	b, err := builtin.ReadFile("presets/" + name + ".json")
	if err != nil {
		return nil, err
	}
	return Parse(name, b)
}

// GetAll returns the built-in presets called names, in order. A name given
// twice is refused, as is one no preset has.
func GetAll(names []string) ([]*Preset, error) {
	presets := make([]*Preset, 0, len(names))
	for i, n := range names {
		if slices.Contains(names[:i], n) {
			return nil, fmt.Errorf("the preset %s is given twice. Give it once", n)
		}
		p, err := Get(n)
		if err != nil {
			return nil, err
		}
		presets = append(presets, p)
	}
	return presets, nil
}

// Parse reads the preset called name from its JSON and checks it. A field
// salt does not know is refused, so a misspelt one never drops a rule.
func Parse(name string, b []byte) (*Preset, error) {
	var p Preset
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("preset %s: %w", name, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("preset %s: there is more after the preset", name)
	}
	if err := p.check(name); err != nil {
		return nil, fmt.Errorf("preset %s: %w", name, err)
	}
	return &p, nil
}

func (p *Preset) check(name string) error {
	if p.Name != name {
		return fmt.Errorf("its name is %q, not the file's name", p.Name)
	}
	if len(p.Paths) == 0 {
		return errors.New("it has no paths")
	}
	for _, x := range p.Paths {
		if x.From == "" {
			return fmt.Errorf("a path has no from")
		}
		if err := checkFrom(x.From); err != nil {
			return err
		}
		clean, err := repo.CleanPath(x.To)
		if err != nil || clean != x.To {
			return fmt.Errorf("the path %q cannot be used in the backup", x.To)
		}
		if strings.Count(x.From, "*") != stars(x.From) || stars(x.From) != stars(x.To) || strings.Count(x.To, "*") != stars(x.To) {
			return fmt.Errorf("%s and %s must have the same number of *, each a whole part of the path", x.From, x.To)
		}
	}
	for _, s := range p.Secrets {
		if len(s.Files) == 0 {
			return errors.New("a secrets rule needs files")
		}
		for _, k := range s.Keys {
			if k != strings.ToLower(k) {
				return fmt.Errorf("the secrets key %q must be in lower case, as settings are matched in lower case", k)
			}
		}
	}
	for _, pat := range slices.Concat(p.Skip, secretPatterns(p.Secrets)) {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("bad pattern %q", pat)
		}
	}
	return nil
}

// PlaceOf returns the place the backup path rel is in, among the paths the
// presets back up: as many of rel's first parts as a path's To has, each *
// in To standing for any one part. When such places nest, the outermost is
// taken. ok is false when no preset backs rel up.
func PlaceOf(presets []*Preset, rel string) (place string, ok bool) {
	parts := strings.Split(rel, "/")
	n := 0
	for _, p := range presets {
		for _, x := range p.Paths {
			to := strings.Split(x.To, "/")
			if len(to) > len(parts) || (ok && len(to) >= n) {
				continue
			}
			if slices.EqualFunc(to, parts[:len(to)], func(t, part string) bool { return t == "*" || t == part }) {
				place, ok, n = strings.Join(parts[:len(to)], "/"), true, len(to)
			}
		}
	}
	return place, ok
}

// stars counts the parts of the slash path p that are only *.
func stars(p string) int {
	n := 0
	for part := range strings.SplitSeq(p, "/") {
		if part == "*" {
			n++
		}
	}
	return n
}

func secretPatterns(ss []Secret) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Files...)
		out = append(out, s.Keys...)
	}
	return out
}

// matchAny reports whether name matches one of the patterns, which check has
// already found valid.
func matchAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}
