package preset

import (
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/seal"
)

// Every built-in preset loads, and backs up each path under its own name, so
// two of its paths never clash in the backup.
func TestBuiltinPresetsAreValid(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no built-in presets")
	}
	for _, n := range names {
		p, err := Get(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if p.About == "" {
			t.Errorf("%s: no about", n)
		}
		if i, j, ok := seal.FirstClash(tos(p), true); ok {
			t.Errorf("%s: %s and %s clash", n, tos(p)[j], tos(p)[i])
		}
	}
}

// Every built-in preset can be given with every other, so no two presets'
// paths clash in the backup. This is a smoke test: a * is compared as it is,
// not as each name it can match, and it refuses one path inside another,
// which Gather allows.
func TestBuiltinPresetsCombine(t *testing.T) {
	presets, err := GetAll(Names())
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, p := range presets {
		all = append(all, tos(p)...)
	}
	if i, j, ok := seal.FirstClash(all, true); ok {
		t.Errorf("%s and %s clash", all[j], all[i])
	}
}

// tos lists the backup path of each of p's paths and databases.
func tos(p *Preset) []string {
	var out []string
	for _, x := range p.Paths {
		out = append(out, x.To)
	}
	for _, d := range p.Databases {
		out = append(out, d.To)
	}
	return out
}

// defaultSecretKeys holds patterns that path.Match can use, in lower case,
// as settings are matched in lower case.
func TestDefaultSecretKeysAreValid(t *testing.T) {
	for _, k := range defaultSecretKeys {
		if _, err := path.Match(k, ""); err != nil || k != strings.ToLower(k) {
			t.Errorf("%q: %v", k, err)
		}
	}
}

// defaultSecretKeys catch a secret's setting written in snake_case or
// camelCase, but not a setting that only ends in key.
func TestDefaultSecretKeysMatch(t *testing.T) {
	for name, want := range map[string]bool{
		"api_key": true, "apikey": true, "secretkey": true, "secret_key": true, "accesskey": true,
		"secretaccesskey": true, "aws_access_key": true, "privatekey": true, "sshprivatekey": true, "private_key": true,
		"mainkey": false, "sessionkey": false, "publickey": false, "cachekey": false,
	} {
		if got := matchAny(defaultSecretKeys, name); got != want {
			t.Errorf("%s: matched %v, want %v", name, got, want)
		}
	}
}

func TestGetUnknownPreset(t *testing.T) {
	_, err := Get("nope")
	if !errors.Is(err, ErrUnknown) || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), Names()[0]) {
		t.Fatalf("Get(nope) = %v", err)
	}
}

func TestNamesAreSorted(t *testing.T) {
	if names := Names(); !slices.IsSorted(names) || slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, ".json") }) {
		t.Fatalf("Names() = %v", names)
	}
}

func TestParseRefusesBadPresets(t *testing.T) {
	for want, body := range map[string]string{
		"unexpected EOF":                       `{`,
		"more after the preset":                `{"name": "t", "paths": [{"from": "/a", "to": "a"}]} {}`,
		"not the file's name":                  `{"name": "other", "paths": [{"from": "/a", "to": "a"}]}`,
		"no paths":                             `{"name": "t"}`,
		"has no from":                          `{"name": "t", "paths": [{"to": "a"}]}`,
		`"../a" cannot be used`:                `{"name": "t", "paths": [{"from": "/a", "to": "../a"}]}`,
		`"/a" cannot be used`:                  `{"name": "t", "paths": [{"from": "/a", "to": "/a"}]}`,
		`"a//b" cannot be used`:                `{"name": "t", "paths": [{"from": "/a", "to": "a//b"}]}`,
		"same number of *":                     `{"name": "t", "paths": [{"from": "/a/*", "to": "a"}]}`,
		"/a/x* and a must have":                `{"name": "t", "paths": [{"from": "/a/x*", "to": "a"}]}`,
		"at most one in a part":                `{"name": "t", "paths": [{"from": "/a/p*q*", "to": "a/*/*"}]}`,
		"/a/*/b and a/x*y* must":               `{"name": "t", "paths": [{"from": "/a/*/b", "to": "a/x*y*"}]}`,
		"*/a: its first part cannot hold a *":  `{"name": "t", "paths": [{"from": "*/a", "to": "*/a"}]}`,
		"p*/a: its first part cannot hold a *": `{"name": "t", "paths": [{"from": "p*/a", "to": "*/a"}]}`,
		"cannot hold a variable":               `{"name": "t", "paths": [{"from": "/a/${B}-*", "to": "a/*"}]}`,
		"${A:-${B}}: a variable":               `{"name": "t", "paths": [{"from": "${A:-${B}}", "to": "a"}]}`,
		"${A:-$B}/a: a variable":               `{"name": "t", "paths": [{"from": "${A:-$B}/a", "to": "a"}]}`,
		"${A:-/x/*/y}: a variable":             `{"name": "t", "paths": [{"from": "${A:-/x/*/y}", "to": "a/*"}]}`,
		"/a/${B: a variable":                   `{"name": "t", "paths": [{"from": "/a/${B", "to": "a"}]}`,
		"rule needs files":                     `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"keys": ["k"]}]}`,
		`bad pattern "["`:                      `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "skip": ["["]}`,
		`bad pattern "[k"`:                     `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "keys": ["[k"]}]}`,
		// A misspelt field would otherwise be dropped, and with it a rule.
		`unknown field "secret"`: `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secret": [{"files": ["x"], "keys": ["k"]}]}`,
		`unknown field "key"`:    `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "key": ["k"]}]}`,
		// A database file is found by its path, so only a kind given by a
		// connection can be named.
		`"sqlite" is not a kind of database a preset can name`:       `{"name": "t", "databases": [{"kind": "sqlite", "from": "/a.db", "to": "a.db"}]}`,
		`"postgres-env" is not a kind of database a preset can name`: `{"name": "t", "databases": [{"kind": "postgres-env", "from": "DB", "to": "a.sql"}]}`,
		`"mysql" is not a kind of database a preset can name`:        `{"name": "t", "databases": [{"kind": "mysql", "from": "mysql://h/a", "to": "a.sql"}]}`,
		`"" is not a kind of database a preset can name`:             `{"name": "t", "databases": [{"from": "postgresql://h/a", "to": "a.sql"}]}`,
		"a database has no from":                                     `{"name": "t", "databases": [{"kind": "postgres", "to": "a.sql"}]}`,
		"postgresql://h/* cannot have a *":                           `{"name": "t", "databases": [{"kind": "postgres", "from": "postgresql://h/*", "to": "a.sql"}]}`,
		"postgresql://h/a* cannot have a *":                          `{"name": "t", "databases": [{"kind": "postgres", "from": "postgresql://h/a*", "to": "a.sql"}]}`,
		"${A:-$B}: a variable":                                       `{"name": "t", "databases": [{"kind": "postgres", "from": "${A:-$B}", "to": "a.sql"}]}`,
		`database path "../a.sql" cannot be used`:                    `{"name": "t", "databases": [{"kind": "postgres", "from": "${A}", "to": "../a.sql"}]}`,
		`database path "a.sql/" cannot be used`:                      `{"name": "t", "databases": [{"kind": "postgres", "from": "${A}", "to": "a.sql/"}]}`,
		`database path "a/*/b.sql" cannot be used`:                   `{"name": "t", "databases": [{"kind": "postgres", "from": "${A}", "to": "a/*/b.sql"}]}`,
		`database path "a/b*.sql" cannot be used`:                    `{"name": "t", "databases": [{"kind": "postgres", "from": "${A}", "to": "a/b*.sql"}]}`,
		`database path "" cannot be used`:                            `{"name": "t", "databases": [{"kind": "postgres", "from": "${A}"}]}`,
		`unknown field "form"`:                                       `{"name": "t", "databases": [{"kind": "postgres", "form": "${A}", "to": "a.sql"}]}`,
		// Settings are matched in lower case, so API_KEY would never match.
		`"*API_KEY" must be in lower case`:                      `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "keys": ["*API_KEY"]}]}`,
		`ref setting "ID" must be an exact name in lower case`:  `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "refs": [["source", "ID"]]}]}`,
		`ref setting "*id" must be an exact name in lower case`: `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "refs": [["source", "*id"]]}]}`,
		`ref setting "i[d" must be an exact name in lower case`: `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "refs": [["source", "i[d"]]}]}`,
		`ref [] must name settings`:                             `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "refs": [[]]}]}`,
		`ref ["id" "id"] must name settings, each once`:         `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "refs": [["id", "id"]]}]}`,
	} {
		_, err := Parse("t", []byte(body))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "preset t: ") {
			t.Errorf("Parse(%s) = %v, want %q", body, err, want)
		}
	}
}

func TestParseGoodPreset(t *testing.T) {
	p, err := Parse("t", []byte(`{"name": "t", "about": "test", "paths": [{"from": "${HOME_X:-~/x}/*/data", "to": "x/*/data"}],
		"skip": ["*.log"], "secrets": [{"files": ["*.yaml"], "keys": ["*api_key"], "refs": [["source", "id"]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Paths[0].To != "x/*/data" || p.Skip[0] != "*.log" || p.Secrets[0].Keys[0] != "*api_key" || !slices.Equal(p.Secrets[0].Refs[0], []string{"source", "id"}) {
		t.Fatalf("Parse = %+v", p)
	}
	// A * may have text around it in a part, in From and in To, and be in a
	// part of To that does not hold one in From.
	if _, err := Parse("t", []byte(`{"name": "t", "paths": [{"from": "${X:-~/x}/.x-*/w-*.d", "to": "x/*/workspace-*"}]}`)); err != nil {
		t.Fatal(err)
	}
	// A preset may name only databases. A connection's default may hold
	// slashes, a colon and an @.
	p, err = Parse("t", []byte(`{"name": "t", "databases": [{"kind": "postgres", "from": "${DB_URL:-postgresql://u:p@h:5432/db}", "to": "t/db.sql"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Database{Kind: "postgres", From: "${DB_URL:-postgresql://u:p@h:5432/db}", To: "t/db.sql"}); len(p.Databases) != 1 || p.Databases[0] != want {
		t.Fatalf("Parse = %+v", p)
	}
}

// A backup path is in the place a preset path's To names, each part holding
// a * matching the parts it can fill, and the innermost place when they
// nest.
func TestPlaceOf(t *testing.T) {
	p, err := Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "~/tool/profiles/*/data", "to": "tool/profiles/*/data"},
		{"from": "~/tool/ws-*", "to": "tool/workspace-*"},
		{"from": "~/tool/settings.yaml", "to": "tool/settings.yaml"},
		{"from": "~/inner", "to": "outer/inner"},
		{"from": "~/outer", "to": "outer"}],
		"databases": [{"kind": "postgres", "from": "${DB}", "to": "tool/memory.sql"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"tool/profiles/work/data/notes.md":  "tool/profiles/work/data",
		"tool/profiles/work/data":           "tool/profiles/work/data",
		"tool/settings.yaml":                "tool/settings.yaml",
		"tool/workspace-a/MEMORY.md":        "tool/workspace-a",
		"tool/workspace-/MEMORY.md":         "",
		"tool/workspace/MEMORY.md":          "",
		"outer/inner/a.md":                  "outer/inner",
		"outer/b.md":                        "outer",
		"tool/profiles/work/other/notes.md": "",
		"tool/profiles/work":                "",
		"tool/settings.yaml.bak":            "",
		"tool/memory.sql":                   "tool/memory.sql",
		"tool/memory.sql.bak":               "",
		"elsewhere.md":                      "",
	} {
		got, ok := PlaceOf([]*Preset{p}, rel)
		if got != want || ok != (want != "") {
			t.Errorf("PlaceOf(%s) = %q, %v, want %q", rel, got, ok, want)
		}
	}
	if _, ok := PlaceOf(nil, "outer/b.md"); ok {
		t.Error("no presets back up nothing")
	}
}

// GetAll returns the presets named, in order, and refuses a name given twice
// or one no preset has.
func TestGetAll(t *testing.T) {
	names := Names()
	got, err := GetAll(names)
	if err != nil || len(got) != len(names) {
		t.Fatalf("GetAll(%v) = %v, %v", names, got, err)
	}
	for i, p := range got {
		if p.Name != names[i] {
			t.Errorf("preset %d is %s, want %s", i, p.Name, names[i])
		}
	}
	if _, err := GetAll([]string{names[0], names[0]}); err == nil || err.Error() != "the preset "+names[0]+" is given twice. Give it once" {
		t.Errorf("twice: %v", err)
	}
	if _, err := GetAll([]string{names[0], "nope"}); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
	if got, err := GetAll(nil); err != nil || len(got) != 0 {
		t.Errorf("none: %v, %v", got, err)
	}
}
