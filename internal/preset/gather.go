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
	// Roots lists every file and folder the presets found.
	Roots []string
}

// LeftOut is a file left out of the backup, and why.
type LeftOut struct {
	Path, Why string
}

// Gather finds everything the presets back up on this machine. show is how
// paths are written in messages. A preset that finds nothing is an error.
// A place found twice, such as a folder named by two paths, is backed up
// only the first time.
func (e Env) Gather(presets []*Preset, show func(string) string) (*Found, error) {
	f := &Found{}
	var taken []string
	for _, p := range presets {
		before := len(f.Files) + len(f.Databases)
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
				if slices.ContainsFunc(taken, func(t string) bool { return seal.Within(real, t) }) {
					continue
				}
				taken = append(taken, real)
				f.Roots = append(f.Roots, real)
				if err := f.walk(p, real, pl, show); err != nil {
					return nil, err
				}
			}
		}
		if len(f.Files)+len(f.Databases) == before {
			return nil, fmt.Errorf("%w for the %s preset. It looks in %s", ErrNothing, p.Name, strings.Join(looked, ", "))
		}
	}
	return f, nil
}

// walk adds the file or folder pl, which is at real once symlinks are
// followed, using p's rules. Paths are given as pl names them.
func (f *Found) walk(p *Preset, real string, pl Place, show func(string) string) error {
	databases := map[string]bool{}
	return filepath.WalkDir(real, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, err := filepath.Rel(real, walked)
		if err != nil {
			return err
		}
		at, to := filepath.Join(pl.Abs, sub), path.Join(pl.Rel, filepath.ToSlash(sub))
		// A path found through a symlink has two names, and rules match
		// either.
		names := []string{d.Name(), filepath.Base(at)}
		if walked != real && (slices.ContainsFunc(names, func(n string) bool {
			return slices.Contains(seal.DefaultExclude, n) || matchAny(p.Skip, n)
		})) {
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
		if slices.ContainsFunc(sidecars, func(s string) bool {
			return strings.HasSuffix(at, s) && databases[strings.TrimSuffix(at, s)]
		}) {
			return nil
		}
		for _, s := range p.Secrets {
			if !slices.ContainsFunc(names, func(n string) bool { return matchAny(s.Files, n) }) {
				continue
			}
			if why := secretIn(at, s.Keys); why != "" {
				f.LeftOut = append(f.LeftOut, LeftOut{Path: at, Why: why})
				return nil
			}
		}
		isDB, err := source.IsSQLite(at)
		if err != nil {
			return err
		}
		if isDB {
			db, err := source.NewSQLite(at)
			if err == nil {
				db, err = source.Named(db, to)
			}
			if err != nil {
				return err
			}
			databases[at] = true
			f.Databases = append(f.Databases, db)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f.Files = append(f.Files, seal.Extra{Rel: to, Path: at, Mode: info.Mode(), ModTime: info.ModTime()})
		return nil
	})
}

// secretIn says why the file at p must be left out: one of the settings
// keys names holds a value, or the file could not be read as YAML to check.
// It returns "" when the file can be backed up.
func secretIn(p string, keys []string) string {
	file, err := os.Open(p)
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err)
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxSecretsFile+1))
	if err != nil {
		return fmt.Sprintf("it could not be read to check it for secrets (%v)", err)
	}
	if len(b) > maxSecretsFile {
		return "it is too large to check for secrets"
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return "it could not be read as YAML to check it for secrets"
		}
		if key := secretKey(&doc, keys); key != "" {
			return fmt.Sprintf("its setting %s holds a secret", key)
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

// hasValue reports whether n holds anything. An alias counts, since what it
// points to is not followed.
func hasValue(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value != "" && n.Tag != "!!null"
	case yaml.AliasNode:
		return true
	}
	return len(n.Content) > 0
}
