package preset

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/spicy-lemonade/salt/internal/proc"
	"github.com/spicy-lemonade/salt/internal/regular"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
)

// maxSecretsFile caps how much of a file is read to check it for secrets.
// A larger file is left out.
const maxSecretsFile = 1 << 20

// Why a file checked for secrets is left out when salt cannot read every
// setting in it.
const (
	notYAML   = "it could not be read as YAML or JSON to check it for secrets"
	commented = "a comment in it stops it being checked for secrets"
	noSpace   = "a name with no space after its colon stops it being checked for secrets"
)

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
	// Databases are the SQLite databases found and the databases the
	// presets name, each copied safely before sealing.
	Databases []source.Database
	// LeftOut lists files left out because they hold, or may hold, secrets.
	LeftOut []LeftOut
	// Skipped lists what is neither a file nor a folder, such as a symlink.
	Skipped []string
	// Numbers lists settings named like a secret that hold a number and no
	// text, in files that are backed up. Secrets almost always mix letters
	// and digits, so a number is not taken for one, but the person is told.
	Numbers []Setting
	// Places lists the slash path of every place found and still there when
	// it was walked, so one backed up last time and missing now can be named.
	Places []string
	// by lists, for each preset, the slash path of every file and database
	// it backs up.
	by map[*Preset][]string
}

// Drop removes the backup paths in gone, of files and databases deleted
// before they were sealed, from f's files, databases and places and from
// what each preset backs up, so f then holds only what the backup does. It
// is for use once f has been sealed: it reuses the memory of f's lists, so
// any copy of them, such as the files given to seal, changes too.
func (f *Found) Drop(gone []string) {
	if len(gone) == 0 {
		return
	}
	set := make(map[string]bool, len(gone))
	for _, rel := range gone {
		set[rel] = true
	}
	f.Files = slices.DeleteFunc(f.Files, func(x seal.Extra) bool { return set[x.Rel] })
	f.Databases = slices.DeleteFunc(f.Databases, func(d source.Database) bool { return set[d.Name()] })
	f.Places = slices.DeleteFunc(f.Places, func(rel string) bool { return set[rel] })
	// A preset left with nothing keeps its entry, empty, so CheckGone finds it.
	for p, rels := range f.by {
		f.by[p] = slices.DeleteFunc(rels, func(rel string) bool { return set[rel] })
	}
}

// CheckGone returns ErrNothing for the first preset, in name order, that
// Drop left with nothing, as every file it found was deleted before it was
// sealed. Such a preset then fails as one that found nothing does.
func (f *Found) CheckGone() error {
	presets := slices.SortedFunc(maps.Keys(f.by), func(a, b *Preset) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range presets {
		if len(f.by[p]) == 0 {
			return fmt.Errorf("%w for the %s preset. What it found was deleted before it could be backed up", ErrNothing, p.Name)
		}
	}
	return nil
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
// paths are written in messages. A preset that finds nothing is an error,
// which names where it looks, and each variable it needs that is not set.
// Each place is walked once. A place found twice, such as a folder named by
// two paths or by two presets, is backed up under the first path, taking
// presets in name order. A place inside another is backed up under its own
// path, and the outer walk leaves it out. Where presets overlap, a file is
// backed up if any preset that reaches it would back it up, and every
// preset's secrets rules apply to every file, so the result never depends
// on the order the presets are given in. A place that contains the backup
// repo at repo, or is inside it, is refused before anything is read. A file
// or place deleted while it is gathered, as a tool's files can be at any
// time, is skipped, and every file is marked live, so seal skips one deleted
// before it is read too. A place skipped this way is left out of Places, and
// a preset left with nothing is an error. A database a preset names is
// backed up when its connection's variables are set or have defaults. One
// named twice, by the same connection without its password, is backed up
// once, under the first name, taking presets in name order.
func (e Env) Gather(presets []*Preset, repo string, show func(string) string) (*Found, error) {
	// Places have their symlinks followed, so the repo's are too before
	// comparing them.
	repoReal, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, err
	}
	presets = slices.SortedFunc(slices.Values(presets), func(a, b *Preset) int { return strings.Compare(a.Name, b.Name) })
	w := &walker{f: &Found{by: map[*Preset][]string{}}, spots: map[string]*spot{}, show: show}
	var spots []*spot
	lookedIn := map[*Preset][]string{}
	for _, p := range presets {
		for _, s := range p.Secrets {
			w.secrets = append(w.secrets, Secret{Files: s.Files, Keys: slices.Concat(defaultSecretKeys, s.Keys), Refs: s.Refs})
		}
		var looked []string
		for _, x := range p.Paths {
			if where, ok := e.expand(x.From); ok {
				looked = append(looked, show(where))
			} else if _, _, missing := e.vars(x.From); len(missing) > 0 {
				looked = append(looked, notSet(missing))
			}
			places, err := e.Find(x)
			if err != nil {
				return nil, err
			}
			for _, pl := range places {
				real := pl.Real
				if s, ok := w.spots[real]; ok {
					s.rels = append(s.rels, pl.Rel)
					if !slices.Contains(s.users, p) {
						s.users = append(s.users, p)
					}
					continue
				}
				s := &spot{place: pl, real: real, rels: []string{pl.Rel}, users: []*Preset{p}}
				w.spots[real] = s
				spots = append(spots, s)
			}
		}
		for _, d := range p.Databases {
			db, missing, err := e.database(p, d)
			if err != nil {
				return nil, err
			}
			if db == nil {
				if len(missing) > 0 {
					looked = append(looked, "the database in "+notSet(missing))
				}
				continue
			}
			w.addDatabase(p, db)
		}
		lookedIn[p] = looked
	}
	for _, s := range spots {
		if err := seal.CheckDisjoint(s.real, repoReal, show); err != nil {
			return nil, err
		}
	}
	for _, s := range spots {
		gone, err := w.walk(s, reaching(s, spots))
		if err != nil {
			return nil, err
		}
		if !gone {
			w.f.Places = append(w.f.Places, s.rels...)
		}
	}
	for _, p := range presets {
		if len(w.f.by[p]) == 0 {
			hint := ""
			if len(w.f.by) > 0 { // another preset found something
				hint = ". If you don't use it, leave out --preset " + p.Name
			}
			return nil, fmt.Errorf("%w for the %s preset. It looks in %s%s", ErrNothing, p.Name, strings.Join(lookedIn[p], ", "), hint)
		}
	}
	return w.f, nil
}

// notSet writes the variables names, which are not set, for the list of
// where a preset looks.
func notSet(names []string) string {
	return "$" + strings.Join(names, " and $") + " (not set)"
}

// database returns the database d names for the preset p, ready to copy.
// It returns nil, and the variables it needs that are not set, when d's
// connection needs one of them. Errors name the variables the connection
// came from, never the connection, which may hold a password. A variable
// that is set comes first, as what it holds is the likelier cause, then
// those whose defaults were used.
func (e Env) database(p *Preset, d Database) (db source.Database, missing []string, err error) {
	conn, err := d.conn()
	if err != nil {
		return nil, nil, fmt.Errorf("preset %s: %w", p.Name, err)
	}
	set, defaulted, missing := e.vars(d.From)
	from, ok := e.expand(d.From)
	if !ok {
		return nil, missing, nil
	}
	where, about := fmt.Sprintf("the %s preset's database connection", p.Name), ""
	if len(set) > 0 {
		where = fmt.Sprintf("the connection in %s, used by the %s preset,", strings.Join(set, " and "), p.Name)
		about = fmt.Sprintf("The %s preset read this connection from %s", p.Name, strings.Join(set, " and "))
	}
	if len(defaulted) > 0 {
		names, are, s := strings.Join(defaulted, " and "), "is", ""
		if len(defaulted) > 1 {
			are, s = "are", "s"
		}
		if about == "" {
			where = fmt.Sprintf("the %s preset's default database connection", p.Name)
			about = fmt.Sprintf("This is the %s preset's default connection, used when %s %s not set", p.Name, names, are)
		} else {
			about += fmt.Sprintf(", with its default%s for %s, which %s not set", s, names, are)
		}
		about += fmt.Sprintf(". If the database is elsewhere, set %s, in the cron line too", names)
	}
	db, err = conn(from, where)
	if err == nil {
		db, err = source.Named(db, d.To)
	}
	if err != nil {
		return nil, nil, err
	}
	return presetDB{Database: db, preset: p.Name, about: about}, nil, nil
}

// presetDB is a database a preset names. Its option is the preset, and when
// its program fails, as when the server cannot be reached, the error says
// where its connection came from.
type presetDB struct {
	source.Database
	preset string
	// about says where the connection came from, or is "" when it holds
	// no variables.
	about string
}

func (d presetDB) Flag() string { return "--preset " + d.preset }

func (d presetDB) Copy(ctx context.Context, o source.CopyOptions) (source.Meta, error) {
	meta, err := d.Database.Copy(ctx, o)
	var failed *proc.Error
	if d.about != "" && errors.As(err, &failed) {
		sep := ". "
		if msg := err.Error(); strings.ContainsAny(msg[len(msg)-1:], ".?!") {
			sep = " "
		}
		err = fmt.Errorf("%w%s%s", err, sep, d.about)
	}
	return meta, err
}

// spot is a place a preset found, once symlinks are followed. rels lists
// the slash path of every place found there, the first being place.Rel, and
// users every preset that found it. It is walked once, under place's path.
type spot struct {
	place Place
	real  string
	rels  []string
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
	// secrets holds every preset's secrets rules, each with
	// defaultSecretKeys added to its keys.
	secrets []Secret
	show    func(string) string
}

// addDatabase adds db, which the preset p names, to what is backed up. A
// database from the same connection, compared without its password, is
// backed up once, under the name it was first added with, and counts for
// p too.
func (w *walker) addDatabase(p *Preset, db source.Database) {
	if i := slices.IndexFunc(w.f.Databases, func(o source.Database) bool { return o.String() == db.String() }); i >= 0 {
		db = w.f.Databases[i]
	} else {
		w.f.Databases = append(w.f.Databases, db)
	}
	if !slices.Contains(w.f.by[p], db.Name()) {
		w.f.by[p] = append(w.f.by[p], db.Name())
	}
}

// walk adds the file or folder in the spot s, which the presets by back up.
// A file or folder is backed up when any of by that reaches it does not
// skip it. Files are read from their real paths, under the one checked
// against the repo, and messages give paths as s's place names them. A
// place in spots inside it is left to its own walk, so it is backed up
// once, under its own path. A file or folder deleted since it was found,
// as a tool's files can be at any time, is skipped, and gone is true when
// that is s itself.
func (w *walker) walk(s *spot, by []*Preset) (gone bool, err error) {
	pl, real := s.place, s.real
	// reach holds, for each folder walked, the presets that reach it.
	reach := map[string][]*Preset{}
	// skipGone returns nil for an error saying walked no longer exists, and
	// any other error.
	skipGone := func(walked string, err error) error {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		gone = gone || walked == real
		return nil
	}
	err = filepath.WalkDir(real, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			return skipGone(walked, err)
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
		sidecar, err := isSidecar(walked)
		if err != nil {
			return skipGone(walked, err)
		}
		if sidecar {
			return nil
		}
		// A secrets file pattern is matched against as many of the last parts
		// of the file's path as it has, so it can name the folders the file
		// is in. The place itself may be a symlink, so its path as the preset
		// names it and its real path are both tried. Inside it, symlinks are
		// not followed.
		paths := []string{filepath.ToSlash(walked), filepath.ToSlash(at)}
		matches := func(pat string) bool {
			for _, p := range paths {
				start := len(p)
				for i := 0; i <= strings.Count(pat, "/") && start >= 0; i++ {
					start = strings.LastIndexByte(p[:start], '/')
				}
				if ok, _ := path.Match(pat, p[start+1:]); ok {
					return true
				}
			}
			return false
		}
		var number string
		for _, sec := range w.secrets {
			if !slices.ContainsFunc(sec.Files, matches) {
				continue
			}
			why, secret, num := secretIn(walked, sec)
			if why != "" {
				w.f.LeftOut = append(w.f.LeftOut, LeftOut{Path: at, Why: why, Secret: secret})
				return nil
			}
			number = cmp.Or(number, num)
		}
		// added records what one more file found tells: which presets back
		// something up, and a number in it named like a secret.
		added := func() {
			for _, p := range here {
				w.f.by[p] = append(w.f.by[p], to)
			}
			if number != "" {
				w.f.Numbers = append(w.f.Numbers, Setting{Path: at, Key: number})
			}
		}
		isDB, err := source.IsSQLite(walked)
		if err != nil {
			return skipGone(walked, err)
		}
		if isDB {
			db, err := source.NewSQLite(walked)
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
		w.f.Files = append(w.f.Files, seal.Extra{Rel: to, Path: walked, Live: true})
		added()
		return nil
	})
	return gone, err
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
// sec's keys name holds a value, and secret is true, or the file could not
// be read as YAML or JSON to check. It returns "" when the file can be
// backed up, or when it no longer exists, which the caller then finds and
// skips. number is the first setting sec's keys name that holds a number
// and no text, which is not taken for a secret.
func secretIn(p string, sec Secret) (why string, secret bool, number string) {
	file, _, err := regular.Open(os.OpenFile, p)
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
			return notYAML, false, ""
		}
		// A comment before the opening brace, with no ": " after it, makes
		// the whole document one piece of text holding settings YAML cannot
		// see.
		if isComment(doc.Content[0]) {
			return commented, false, ""
		}
		text, num, unread := secretKeys(&doc, sec)
		if unread != "" {
			return unread, false, ""
		}
		if text != "" {
			return fmt.Sprintf("its setting %s holds a secret", text), true, ""
		}
		number = cmp.Or(number, num)
	}
}

// secretKeys returns the first setting in n, at any depth, whose name
// matches sec's keys in lower case and that holds a value, and the first
// such setting that holds a number and no value, or "" for either. unread,
// with the others "", says why n cannot be checked, when YAML reads JSON5
// so that a setting's name matches no key. A comment joins the name before
// or after it (see isComment). A name and value with no space between them,
// as in {apiKey:"x"}, become one name, so a name not quoted inside {} that
// holds : stops the check, even where YAML means it, as in {llama3:8b: 1}.
// A quoted name, such as "a:b", and a YAML name outside {}, such as
// llama3:8b, are names.
func secretKeys(n *yaml.Node, sec Secret) (text, number, unread string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			t, num, u := secretKeys(c, sec)
			if t != "" || u != "" {
				return t, "", u
			}
			number = cmp.Or(number, num)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if isComment(k) {
				return "", "", commented
			}
			if n.Style&yaml.FlowStyle != 0 && k.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) == 0 && strings.Contains(k.Value, ":") {
				return "", "", noSpace
			}
			if matchAny(sec.Keys, strings.ToLower(k.Value)) {
				if hasValue(v, sec.Refs) {
					return k.Value, "", ""
				}
				if hasNumber(v) {
					number = cmp.Or(number, k.Value)
				}
			}
			t, num, u := secretKeys(v, sec)
			if t != "" || u != "" {
				return t, "", u
			}
			number = cmp.Or(number, num)
		}
	}
	return "", number, ""
}

// isComment reports whether n is text, not quoted, that starts with // or
// holds /*, as YAML reads a JSON5 comment joined to what is around it. A
// quoted name, such as package.json's "//" or "src/*", is not a comment.
func isComment(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) == 0 &&
		(strings.HasPrefix(n.Value, "//") || strings.Contains(n.Value, "/*"))
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

// envRef matches a string that is only ${NAME} or ${env:NAME}, which names
// the environment variable a secret is kept in, not the secret.
var envRef = regexp.MustCompile(`^\$\{(env:)?` + varName + `\}$`)

// hasValue reports whether n holds text that could be a secret: a string
// that is not empty, at any depth. A number, true or false, or null is a
// setting, never a secret, so max_token: 512 is not taken for one. Nor is a
// string that is only ${NAME} or ${env:NAME}, though one with anything more,
// such as a default, is, or an object refs names (see isRef). An alias
// counts, since what it points to is not followed.
func hasValue(n *yaml.Node, refs [][]string) bool {
	has := func(c *yaml.Node) bool { return hasValue(c, refs) }
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!binary":
			return n.Value != ""
		case "!!str":
			return n.Value != "" && !envRef.MatchString(n.Value)
		}
		return false
	case yaml.AliasNode:
		return true
	case yaml.MappingNode:
		if isRef(n, refs) {
			return false
		}
		for i := 1; i < len(n.Content); i += 2 {
			if has(n.Content[i]) {
				return true
			}
		}
		return false
	}
	return slices.ContainsFunc(n.Content, has)
}

// isRef reports whether the mapping n names where a secret is kept, as
// {source: env, id: NAME} can: it holds exactly the settings one of refs
// lists, compared in lower case, and nothing but one value in each.
func isRef(n *yaml.Node, refs [][]string) bool {
	if slices.ContainsFunc(n.Content, func(c *yaml.Node) bool { return c.Kind != yaml.ScalarNode }) {
		return false
	}
	names := make([]string, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		names = append(names, strings.ToLower(n.Content[i].Value))
	}
	return slices.ContainsFunc(refs, func(ref []string) bool {
		return len(ref) == len(names) && !slices.ContainsFunc(ref, func(k string) bool { return !slices.Contains(names, k) })
	})
}
