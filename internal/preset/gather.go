package preset

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
)

// maxSecretsFile caps how much of a file is read to check it for secrets.
// A larger file is left out.
const maxSecretsFile = 1 << 20

// sidecars are the files SQLite keeps beside a database while it is in use.
// A safe copy of the database already holds what is in them.
var sidecars = []string{"-wal", "-shm", "-journal"}

// listedHook lets tests delete a file or folder once the walk has listed
// it. It is always nil outside tests.
var listedHook func(path string)

// ErrNothing means a preset found nothing to back up on this machine.
var ErrNothing = errors.New("found nothing to back up")

// Found is everything the presets back up from this machine.
type Found struct {
	// Files are sealed as they are.
	Files []seal.Extra
	// Databases are SQLite databases, copied safely before sealing.
	Databases []source.Database
	// LeftOut lists files left out because they hold, or may hold, secrets.
	LeftOut []LeftOut
	// Skipped lists what is neither a file nor a folder, such as a symlink.
	Skipped []string
	// Numbers lists settings named like a secret that hold a number and no
	// text, in files that are backed up. Secrets almost always mix letters
	// and digits, so a number is not taken for one, but the person is told.
	Numbers []Setting
	// Places lists the slash path of every place found, so one backed up
	// last time and missing now can be named.
	Places []string
}

// Paths lists the slash path every file and database is backed up under.
func (f *Found) Paths() []string {
	paths := make([]string, 0, len(f.Files)+len(f.Databases))
	for _, x := range f.Files {
		paths = append(paths, x.Rel)
	}
	for _, d := range f.Databases {
		paths = append(paths, d.Name())
	}
	return paths
}

// Setting names the setting Key in the file at Path.
type Setting struct{ Path, Key string }

// LeftOut is a file left out of the backup, and why. Secret is true when a
// setting in it holds a secret, and false when it could not be checked.
type LeftOut struct {
	Path, Why string
	Secret    bool
}

// Gather finds everything the presets back up on this machine. show is how
// paths are written in messages. A preset that finds nothing is an error.
// Each place is walked once. A place found twice, such as a folder named by
// two paths or by two presets, is backed up under the first path, taking
// presets in name order. A place inside another is backed up under its own
// path, and the outer walk leaves it out. Where presets overlap, a file is
// backed up if any preset that reaches it would back it up, and every
// preset's secrets rules apply to every file, so the result never depends
// on the order the presets are given in. A place that contains the backup
// repo at repo, or is inside it, is refused before anything is read. A file
// deleted while it is gathered, as a tool's files can be at any time, is
// skipped, and every file is marked live, so seal skips one deleted before
// it is read too.
func (e Env) Gather(presets []*Preset, repo string, show func(string) string) (*Found, error) {
	// Places have their symlinks followed, so the repo's are too before
	// comparing them.
	repoReal, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, err
	}
	presets = slices.SortedFunc(slices.Values(presets), func(a, b *Preset) int { return strings.Compare(a.Name, b.Name) })
	w := &walker{f: &Found{}, spots: map[string]*spot{}, found: map[*Preset]bool{}, show: show}
	var spots []*spot
	lookedIn := map[*Preset][]string{}
	for _, p := range presets {
		w.secrets = append(w.secrets, p.Secrets...)
		var looked []string
		for _, x := range p.Paths {
			if where, ok := e.expand(x.From); ok {
				looked = append(looked, show(where))
			}
			places, err := e.Find(x)
			if err != nil {
				return nil, err
			}
			for _, pl := range places {
				w.f.Places = append(w.f.Places, pl.Rel)
				real, err := filepath.EvalSymlinks(pl.Abs)
				if err != nil {
					return nil, err
				}
				if s, ok := w.spots[real]; ok {
					if !slices.Contains(s.users, p) {
						s.users = append(s.users, p)
					}
					continue
				}
				s := &spot{place: pl, real: real, users: []*Preset{p}}
				w.spots[real] = s
				spots = append(spots, s)
			}
		}
		lookedIn[p] = looked
	}
	for _, s := range spots {
		if err := seal.CheckDisjoint(s.real, repoReal, show); err != nil {
			return nil, err
		}
	}
	for _, s := range spots {
		if err := w.walk(s, reaching(s, spots)); err != nil {
			return nil, err
		}
	}
	for _, p := range presets {
		if !w.found[p] {
			return nil, fmt.Errorf("%w for the %s preset. It looks in %s", ErrNothing, p.Name, strings.Join(lookedIn[p], ", "))
		}
	}
	return w.f, nil
}

// spot is a place a preset found, once symlinks are followed. users lists
// every preset that found it. It is walked once, under place's path.
type spot struct {
	place Place
	real  string
	users []*Preset
}

// reaching returns the presets that back up what is in the spot s: those
// that found it, and those that found a place around it and skip none of
// the folders between, including s itself.
func reaching(s *spot, spots []*spot) []*Preset {
	by := slices.Clone(s.users)
	for _, o := range spots {
		if o == s || !seal.Within(s.real, o.real) {
			continue
		}
		rel, err := filepath.Rel(o.real, s.real)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for _, p := range o.users {
			if !slices.Contains(by, p) && !slices.ContainsFunc(parts, func(n string) bool { return skips(p, n) }) {
				by = append(by, p)
			}
		}
	}
	return by
}

// skips reports whether p never backs up a file or folder called name.
func skips(p *Preset, name string) bool {
	return slices.Contains(seal.DefaultExclude, name) || matchAny(p.Skip, name)
}

// walker gathers the files in each spot into f.
type walker struct {
	f *Found
	// spots holds every spot by its real path, so a walk leaves out the
	// places inside it that are walked on their own.
	spots map[string]*spot
	// secrets holds every preset's secrets rules.
	secrets []Secret
	// found records each preset that backs up something found.
	found map[*Preset]bool
	show  func(string) string
}

// walk adds the file or folder in the spot s, which the presets by back up.
// A file or folder is backed up when any of by that reaches it does not
// skip it. Paths are given as s's place names them, and a place in spots
// inside it is left to its own walk, so it is backed up once, under its own
// path.
func (w *walker) walk(s *spot, by []*Preset) error {
	pl, real := s.place, s.real
	// reach holds, for each folder walked, the presets that reach it.
	reach := map[string][]*Preset{}
	return filepath.WalkDir(real, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			if walked == real {
				return err
			}
			return skipGone(err)
		}
		if listedHook != nil {
			listedHook(walked)
		}
		if _, ok := w.spots[walked]; ok && walked != real {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		here := by
		if walked != real {
			here = slices.DeleteFunc(slices.Clone(reach[filepath.Dir(walked)]), func(p *Preset) bool { return skips(p, d.Name()) })
			if len(here) == 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			reach[walked] = here
			return nil
		}
		sub, err := filepath.Rel(real, walked)
		if err != nil {
			return err
		}
		at, to := filepath.Join(pl.Abs, sub), path.Join(pl.Rel, filepath.ToSlash(sub))
		if !d.Type().IsRegular() {
			w.f.Skipped = append(w.f.Skipped, w.show(at))
			return nil
		}
		if sidecar, err := isSidecar(walked); sidecar || err != nil {
			return skipGone(err)
		}
		// The place itself may be a symlink, and then has two names; the
		// secrets rules match either. Inside it, symlinks are not followed.
		names := []string{d.Name()}
		if walked == real {
			names = append(names, filepath.Base(pl.Abs))
		}
		var number string
		for _, sec := range w.secrets {
			if !slices.ContainsFunc(names, func(n string) bool { return matchAny(sec.Files, n) }) {
				continue
			}
			why, secret, num := secretIn(at, sec.Keys)
			if why != "" {
				w.f.LeftOut = append(w.f.LeftOut, LeftOut{Path: at, Why: why, Secret: secret})
				return nil
			}
			number = cmp.Or(number, num)
		}
		// added records what one more file found tells: which presets back
		// something up, and a number in it named like a secret.
		added := func() {
			w.mark(here)
			if number != "" {
				w.f.Numbers = append(w.f.Numbers, Setting{Path: at, Key: number})
			}
		}
		isDB, err := source.IsSQLite(at)
		if err != nil {
			return skipGone(err)
		}
		if isDB {
			db, err := source.NewSQLite(at)
			if err == nil {
				db, err = source.Named(db, to)
			}
			if err != nil {
				return err
			}
			w.f.Databases = append(w.f.Databases, db)
			added()
			return nil
		}
		// Seal reads its permissions and date as it reads it.
		w.f.Files = append(w.f.Files, seal.Extra{Rel: to, Path: at, Live: true})
		added()
		return nil
	})
}

// mark records that each of by backs up something found.
func (w *walker) mark(by []*Preset) {
	for _, p := range by {
		w.found[p] = true
	}
}

// skipGone returns nil for an error saying a file or folder no longer
// exists, so one deleted since the walk listed it is skipped, as a tool's
// files can be at any time. Any other error is returned.
func skipGone(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// isSidecar reports whether the file at p is a -wal, -shm or -journal file
// beside a SQLite database, whose safe copy already holds what is in it.
func isSidecar(p string) (bool, error) {
	for _, s := range sidecars {
		if db, ok := strings.CutSuffix(p, s); ok {
			isDB, err := source.IsSQLite(db)
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil // a leftover with no database beside it
			}
			return isDB, err
		}
	}
	return false, nil
}

// secretIn says why the file at p must be left out: one of the settings
// keys names holds a value, and secret is true, or the file could not be
// read as YAML to check. It returns "" when the file can be backed up, or
// when it no longer exists, which the caller then finds and skips. number
// is the first setting keys names that holds a number and no text, which is
// not taken for a secret.
func secretIn(p string, keys []string) (why string, secret bool, number string) {
	file, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, ""
	}
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err), false, ""
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxSecretsFile+1))
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err), false, ""
	}
	if len(b) > maxSecretsFile {
		return "it is too large to check for secrets", false, ""
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return "", false, number
		}
		if err != nil {
			return "it could not be read as YAML to check it for secrets", false, ""
		}
		text, num := secretKeys(&doc, keys)
		if text != "" {
			return fmt.Sprintf("its setting %s holds a secret", text), true, ""
		}
		number = cmp.Or(number, num)
	}
}

// secretKeys returns the first setting in n, at any depth, whose name
// matches keys in lower case and that holds a value, and the first such
// setting that holds a number and no value, or "" for either.
func secretKeys(n *yaml.Node, keys []string) (text, number string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			t, num := secretKeys(c, keys)
			if t != "" {
				return t, ""
			}
			number = cmp.Or(number, num)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if matchAny(keys, strings.ToLower(k.Value)) {
				if hasValue(v) {
					return k.Value, ""
				}
				if hasNumber(v) {
					number = cmp.Or(number, k.Value)
				}
			}
			t, num := secretKeys(v, keys)
			if t != "" {
				return t, ""
			}
			number = cmp.Or(number, num)
		}
	}
	return "", number
}

// hasNumber reports whether n holds a number, an integer or a decimal, at
// any depth.
func hasNumber(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Tag == "!!int" || n.Tag == "!!float"
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if hasNumber(n.Content[i]) {
				return true
			}
		}
		return false
	}
	return slices.ContainsFunc(n.Content, hasNumber)
}

// hasValue reports whether n holds text that could be a secret: a string
// that is not empty, at any depth. A number, true or false, or null is a
// setting, never a secret, so max_token: 512 is not taken for one. An alias
// counts, since what it points to is not followed.
func hasValue(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value != "" && (n.Tag == "!!str" || n.Tag == "!!binary")
	case yaml.AliasNode:
		return true
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if hasValue(n.Content[i]) {
				return true
			}
		}
		return false
	}
	return slices.ContainsFunc(n.Content, hasValue)
}
