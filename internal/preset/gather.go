package preset

import (
	"bytes"
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

// listedHook lets tests delete a file or folder once the walk has listed it.
// It is always nil outside tests.
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

// LeftOut is a file left out of the backup, and why. Secret is true when a
// setting in it holds a secret, and false when it could not be checked.
type LeftOut struct {
	Path, Why string
	Secret    bool
}

// Gather finds everything the presets back up on this machine. show is how
// paths are written in messages. A preset that finds nothing is an error.
// A place found twice, such as a folder named by two paths or by two
// presets, is backed up only under the first path. A place inside another
// is backed up under its own path, and the outer one leaves it out, so
// nothing is backed up twice whatever order the presets are given in. A
// place that contains the backup repo at repo, or is inside it, is refused
// before anything is read. A file deleted while it is gathered, as a tool's
// files can be at any time, is skipped, and every file is marked live, so
// seal skips one deleted before it is read too.
func (e Env) Gather(presets []*Preset, repo string, show func(string) string) (*Found, error) {
	// Places have their symlinks followed, so the repo's are too before
	// comparing them.
	repoReal, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, err
	}
	f := &Found{}
	byReal := map[string]*spot{}
	var spots []*spot
	lookedIn := map[*Preset][]string{}
	for _, p := range presets {
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
				real, err := filepath.EvalSymlinks(pl.Abs)
				if err != nil {
					return nil, err
				}
				if s, ok := byReal[real]; ok {
					s.users = append(s.users, p)
					continue
				}
				s := &spot{preset: p, place: pl, real: real, users: []*Preset{p}}
				byReal[real] = s
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
		before := len(f.Files) + len(f.Databases)
		if err := f.walk(s.preset, s.real, s.place, byReal, show); err != nil {
			return nil, err
		}
		s.found = len(f.Files) + len(f.Databases) - before
	}
	for _, p := range presets {
		if !slices.ContainsFunc(spots, func(s *spot) bool { return s.found > 0 && slices.Contains(s.users, p) }) {
			return nil, fmt.Errorf("%w for the %s preset. It looks in %s", ErrNothing, p.Name, strings.Join(lookedIn[p], ", "))
		}
	}
	return f, nil
}

// spot is a place a preset found, once symlinks are followed. users lists
// every preset that found it; it is walked once, with preset's rules.
type spot struct {
	preset *Preset
	place  Place
	real   string
	users  []*Preset
	found  int // files and databases its walk added
}

// walk adds the file or folder pl, which is at real once symlinks are
// followed, using p's rules. Paths are given as pl names them. A place in
// spots found inside it is left to its own walk, so it is backed up once,
// under its own path and with its own preset's rules.
func (f *Found) walk(p *Preset, real string, pl Place, spots map[string]*spot, show func(string) string) error {
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
		if _, ok := spots[walked]; ok && walked != real {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		sub, err := filepath.Rel(real, walked)
		if err != nil {
			return err
		}
		at, to := filepath.Join(pl.Abs, sub), path.Join(pl.Rel, filepath.ToSlash(sub))
		if walked != real && (slices.Contains(seal.DefaultExclude, d.Name()) || matchAny(p.Skip, d.Name())) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			f.Skipped = append(f.Skipped, show(at))
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
		for _, s := range p.Secrets {
			if !slices.ContainsFunc(names, func(n string) bool { return matchAny(s.Files, n) }) {
				continue
			}
			if why, secret := secretIn(at, s.Keys); why != "" {
				f.LeftOut = append(f.LeftOut, LeftOut{Path: at, Why: why, Secret: secret})
				return nil
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
			f.Databases = append(f.Databases, db)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return skipGone(err)
		}
		f.Files = append(f.Files, seal.Extra{Rel: to, Path: at, Mode: info.Mode(), ModTime: info.ModTime(), Live: true})
		return nil
	})
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
// when it no longer exists, which the caller then finds and skips.
func secretIn(p string, keys []string) (why string, secret bool) {
	file, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err), false
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxSecretsFile+1))
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err), false
	}
	if len(b) > maxSecretsFile {
		return "it is too large to check for secrets", false
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			return "it could not be read as YAML to check it for secrets", false
		}
		if key := secretKey(&doc, keys); key != "" {
			return fmt.Sprintf("its setting %s holds a secret", key), true
		}
	}
}

// secretKey returns the first setting in n, at any depth, whose name matches
// keys in lower case and that holds a value, or "".
func secretKey(n *yaml.Node, keys []string) string {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if k := secretKey(c, keys); k != "" {
				return k
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if matchAny(keys, strings.ToLower(k.Value)) && hasValue(v) {
				return k.Value
			}
			if found := secretKey(v, keys); found != "" {
				return found
			}
		}
	}
	return ""
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
