package preset

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/proc"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
)

// sqliteFile starts with SQLite's header, so it is found as a database. No
// database program is started.
const sqliteFile = "SQLite format 3\x00rest of the database"

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func plain(p string) string { return p }

// testPreset backs up ~/tool and each ~/profiles/*/tool, skips logs and
// checks settings.yaml for keys.
func testPreset(t *testing.T) *Preset {
	t.Helper()
	p, err := Parse("t", []byte(`{"name": "t", "about": "test",
		"paths": [
			{"from": "~/tool", "to": "tool"},
			{"from": "~/profiles/*/tool", "to": "profiles/*/tool"},
			{"from": "~/single.txt", "to": "single.txt"}
		],
		"skip": ["*.log", "cache"],
		"secrets": [{"files": ["settings.yaml"], "keys": ["*api_key", "token"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rels(f *Found) (files, dbs []string) {
	for _, x := range f.Files {
		files = append(files, x.Rel)
	}
	for _, d := range f.Databases {
		dbs = append(dbs, d.Name())
	}
	slices.Sort(files)
	slices.Sort(dbs)
	return files, dbs
}

func TestGather(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir()) // files are read from real paths
	if err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(home, "tool")
	write(t, filepath.Join(tool, "notes.md"), "notes")
	write(t, filepath.Join(tool, "sub", "deep.md"), "deep")
	write(t, filepath.Join(tool, "memory.db"), sqliteFile)
	write(t, filepath.Join(tool, "memory.db-wal"), "wal")
	write(t, filepath.Join(tool, "memory.db-shm"), "shm")
	write(t, filepath.Join(tool, "memory.db-journal"), "journal")
	write(t, filepath.Join(tool, "orphan.db-wal"), "a -wal file with no database beside it")
	write(t, filepath.Join(tool, "empty.db"), "")
	write(t, filepath.Join(tool, "run.log"), "log")
	write(t, filepath.Join(tool, "cache", "model.bin"), "model")
	write(t, filepath.Join(tool, ".DS_Store"), "finder")
	write(t, filepath.Join(tool, "settings.yaml"), "level: 3\napi_key: \"\"\ntoken: 12345\n")
	write(t, filepath.Join(home, "profiles", "work", "tool", "settings.yaml"), "llm:\n  api_key: sk-123\n")
	write(t, filepath.Join(home, "profiles", "work", "tool", "bank", "memory.db"), sqliteFile)
	write(t, filepath.Join(home, "single.txt"), "one file")
	os.Symlink("notes.md", filepath.Join(tool, "link.md"))

	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	files, dbs := rels(f)
	wantFiles := []string{"single.txt", "tool/empty.db", "tool/notes.md", "tool/orphan.db-wal", "tool/settings.yaml", "tool/sub/deep.md"}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("files = %v, want %v", files, wantFiles)
	}
	if want := []string{"profiles/work/tool/bank/memory.db", "tool/memory.db"}; !slices.Equal(dbs, want) {
		t.Errorf("databases = %v, want %v", dbs, want)
	}
	// Every file is live, so seal reads its permissions and date itself.
	for _, x := range f.Files {
		if !x.Live || x.Path != filepath.Join(home, filepath.FromSlash(x.Rel)) {
			t.Errorf("%s: %+v", x.Rel, x)
		}
	}
	// Databases are SQLite databases, shown by where they are.
	for _, d := range f.Databases {
		if d.Flag() != "--sqlite" || !strings.HasPrefix(d.String(), home) {
			t.Errorf("database %s %s", d.Flag(), d.String())
		}
	}
	if len(f.LeftOut) != 1 || !strings.HasSuffix(f.LeftOut[0].Path, filepath.Join("work", "tool", "settings.yaml")) || f.LeftOut[0].Why != "its setting api_key holds a secret" {
		t.Errorf("left out = %+v", f.LeftOut)
	}
	if len(f.Skipped) != 1 || !strings.HasSuffix(f.Skipped[0], "link.md") {
		t.Errorf("skipped = %v", f.Skipped)
	}
	// The settings file is backed up, and its number named.
	if want := []Setting{{Path: filepath.Join(tool, "settings.yaml"), Key: "token"}}; !slices.Equal(f.Numbers, want) {
		t.Errorf("numbers = %+v, want %+v", f.Numbers, want)
	}
	if want := []string{"tool", "profiles/work/tool", "single.txt"}; !slices.Equal(f.Places, want) {
		t.Errorf("places = %v, want %v", f.Places, want)
	}
}

// Every file a secrets rule names is checked for defaultSecretKeys as well
// as the rule's own keys, and a rule may name no keys of its own. A file no
// rule names is never checked, and the preset itself is left as it was.
func TestGatherDefaultSecretKeys(t *testing.T) {
	home := t.TempDir()
	p, err := Parse("t", []byte(`{"name": "t", "paths": [{"from": "~/tool", "to": "tool"}],
		"secrets": [{"files": ["a.yaml"], "keys": ["custom"]}, {"files": ["b.yaml"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(home, "tool")
	write(t, filepath.Join(tool, "a.yaml"), "password: hunter2\n")
	write(t, filepath.Join(tool, "sub", "a.yaml"), "custom: x\n")
	write(t, filepath.Join(tool, "b.yaml"), "client_secret: x\n")
	write(t, filepath.Join(tool, "sub", "b.yaml"), "model: x\napi_key: ${env:K}\n")
	write(t, filepath.Join(tool, "c.yaml"), "password: hunter2\n")

	f, err := envOf(home, nil).Gather([]*Preset{p}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"tool/c.yaml", "tool/sub/b.yaml"}) {
		t.Errorf("files = %v", files)
	}
	why := map[string]string{}
	for _, l := range f.LeftOut {
		rel, _ := filepath.Rel(tool, l.Path)
		why[filepath.ToSlash(rel)] = l.Why
	}
	want := map[string]string{
		"a.yaml":     "its setting password holds a secret",
		"sub/a.yaml": "its setting custom holds a secret",
		"b.yaml":     "its setting client_secret holds a secret",
	}
	if !maps.Equal(why, want) {
		t.Errorf("left out = %v, want %v", why, want)
	}
	if !slices.Equal(p.Secrets[0].Keys, []string{"custom"}) || p.Secrets[1].Keys != nil {
		t.Errorf("the preset's keys became %v and %v", p.Secrets[0].Keys, p.Secrets[1].Keys)
	}
}

// A place that holds the backup repo, or is inside it, is refused before
// anything in it is read, comparing real paths.
func TestGatherRefusesTheRepo(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "tool", "notes.md"), "notes")
	inside := mkdir(t, home, "tool", "repo")
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(inside, link)
	for name, repo := range map[string]string{"inside a place": link, "around a place": home} {
		_, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, repo, plain)
		if err == nil || !strings.Contains(err.Error(), "must not contain each other") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, filepath.Join(home, "missing"), plain); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing repo: %v", err)
	}
}

// A place found twice, as when a variable names a default folder, is backed
// up only under the first path. A place inside another is backed up under
// its own path, and the folder around it leaves it out.
func TestGatherTakesEachPlaceOnce(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "data", "a.md"), "a")
	write(t, filepath.Join(home, "data", "b.md"), "b")
	p, err := Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "~/data", "to": "first"},
		{"from": "${DATA}", "to": "second"},
		{"from": "~/data/a.md", "to": "third.md"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(filepath.Join(home, "data"), link)
	f, err := envOf(home, map[string]string{"DATA": link}).Gather([]*Preset{p}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"first/b.md", "third.md"}) {
		t.Fatalf("files = %v", files)
	}
	if want := []string{"first", "second", "third.md"}; !slices.Equal(f.Places, want) {
		t.Fatalf("places = %v, want %v", f.Places, want)
	}
}

// Two presets whose places overlap back each file up once, whatever order
// they are given in, and neither finds nothing.
func TestGatherOverlappingPresets(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "agent", "notes.md"), "notes")
	write(t, filepath.Join(home, "agent", "memory", "m.md"), "memory")
	write(t, filepath.Join(home, "agent", "memory", "config.yaml"), "api_key: sk-1")
	outer, err := Parse("outer", []byte(`{"name": "outer", "paths": [{"from": "~/agent", "to": "agent"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := Parse("inner", []byte(`{"name": "inner", "paths": [{"from": "~/agent/memory", "to": "agent/memory"}],
		"secrets": [{"files": ["config.yaml"], "keys": ["api_key"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]*Preset{{outer, inner}, {inner, outer}} {
		f, err := envOf(home, nil).Gather(order, t.TempDir(), plain)
		if err != nil {
			t.Fatalf("%s first: %v", order[0].Name, err)
		}
		if files, _ := rels(f); !slices.Equal(files, []string{"agent/memory/m.md", "agent/notes.md"}) {
			t.Errorf("%s first: files = %v", order[0].Name, files)
		}
		if len(f.LeftOut) != 1 {
			t.Errorf("%s first: the inner preset's secrets rule was not used: %+v", order[0].Name, f.LeftOut)
		}
	}
	// Two presets naming the same place both find it.
	same, err := Parse("same", []byte(`{"name": "same", "paths": [{"from": "~/agent", "to": "same"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f, err := envOf(home, nil).Gather([]*Preset{outer, same}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); len(files) != 3 || !strings.HasPrefix(files[0], "agent/") {
		t.Fatalf("files = %v", files)
	}
}

// Where presets overlap, a file is backed up if any preset that reaches it
// would back it up, so adding a preset never drops a file another backs up.
// A preset reaches a place inside its own unless it skips a folder on the
// way. Every preset's secrets rules apply to every file.
func TestGatherOverlapRules(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "agent", "run.log"), "log")
	write(t, filepath.Join(home, "agent", "memory", "m.md"), "memory")
	write(t, filepath.Join(home, "agent", "memory", "run.log"), "inner log")
	write(t, filepath.Join(home, "agent", "memory", "draft.tmp"), "draft")
	write(t, filepath.Join(home, "agent", "memory", "settings.yaml"), "token: abc")
	write(t, filepath.Join(home, "agent", "cache", "memory", "c.md"), "cached")
	write(t, filepath.Join(home, "agent", "cache", "memory", "c.tmp"), "cached draft")
	outer, err := Parse("outer", []byte(`{"name": "outer", "paths": [{"from": "~/agent", "to": "agent"}],
		"skip": ["*.log", "cache"],
		"secrets": [{"files": ["settings.yaml"], "keys": ["token"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := Parse("inner", []byte(`{"name": "inner", "paths": [
		{"from": "~/agent/memory", "to": "agent/memory"},
		{"from": "~/agent/cache/memory", "to": "cached"}],
		"skip": ["*.tmp"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]*Preset{{outer, inner}, {inner, outer}} {
		f, err := envOf(home, nil).Gather(order, t.TempDir(), plain)
		if err != nil {
			t.Fatalf("%s first: %v", order[0].Name, err)
		}
		// The inner preset backs up the log in its place, and the outer one
		// the draft. The outer one skips cache, so only the inner one's rules
		// apply there.
		want := []string{"agent/memory/draft.tmp", "agent/memory/m.md", "agent/memory/run.log", "cached/c.md"}
		if files, _ := rels(f); !slices.Equal(files, want) {
			t.Errorf("%s first: files = %v, want %v", order[0].Name, files, want)
		}
		if len(f.LeftOut) != 1 || filepath.Base(f.LeftOut[0].Path) != "settings.yaml" {
			t.Errorf("%s first: the outer preset's secrets rule was not used: %+v", order[0].Name, f.LeftOut)
		}
	}
}

// A place two presets name is backed up under the path of the one first by
// name, whatever order they are given in, with both presets' rules.
func TestGatherSamePlaceAnyOrder(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "agent", "a.md"), "a")
	write(t, filepath.Join(home, "agent", "config.yaml"), "api_key: sk-1")
	b, err := Parse("b", []byte(`{"name": "b", "paths": [{"from": "~/agent", "to": "bee"}],
		"secrets": [{"files": ["config.yaml"], "keys": ["api_key"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Parse("a", []byte(`{"name": "a", "paths": [{"from": "~/agent", "to": "ay"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]*Preset{{a, b}, {b, a}} {
		f, err := envOf(home, nil).Gather(order, t.TempDir(), plain)
		if err != nil {
			t.Fatalf("%s first: %v", order[0].Name, err)
		}
		if files, _ := rels(f); !slices.Equal(files, []string{"ay/a.md"}) || len(f.LeftOut) != 1 {
			t.Errorf("%s first: files = %v, left out %+v", order[0].Name, files, f.LeftOut)
		}
	}
}

// A preset whose place holds nothing but another preset's place still finds
// what is in it.
func TestGatherOuterPlaceHoldsOnlyAnother(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "agent", "memory", "m.md"), "memory")
	outer, _ := Parse("outer", []byte(`{"name": "outer", "paths": [{"from": "~/agent", "to": "agent"}]}`))
	inner, _ := Parse("inner", []byte(`{"name": "inner", "paths": [{"from": "~/agent/memory", "to": "memory"}]}`))
	f, err := envOf(home, nil).Gather([]*Preset{outer, inner}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"memory/m.md"}) {
		t.Fatalf("files = %v", files)
	}
}

// A preset that finds nothing says where it looked.
func TestGatherFindsNothing(t *testing.T) {
	home := t.TempDir()
	mkdir(t, home, "tool") // an empty folder holds nothing to back up
	write(t, filepath.Join(home, "profiles", "a", "tool", "settings.yaml"), "token: abc")
	_, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), func(p string) string { return "<" + filepath.Base(p) + ">" })
	// With one preset, leaving it out would leave nothing, so it is not
	// suggested.
	if !errors.Is(err, ErrNothing) || !strings.HasSuffix(err.Error(), "for the t preset. It looks in <tool>, <tool>, <single.txt>") {
		t.Fatalf("Gather = %v", err)
	}
	// With another preset that finds nothing too, the place is more likely
	// wrong than the preset unused, so leaving it out is not suggested.
	other, _ := Parse("other", []byte(`{"name": "other", "paths": [{"from": "${UNSET}", "to": "o"}]}`))
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t), other}, t.TempDir(), plain); !errors.Is(err, ErrNothing) || strings.Contains(err.Error(), "leave out") {
		t.Fatalf("Gather = %v", err)
	}
	// The second preset is checked too, and with another that finds
	// something, leaving it out is suggested.
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t), other}, t.TempDir(), plain); !errors.Is(err, ErrNothing) || !strings.HasSuffix(err.Error(), "other preset. It looks in $UNSET (not set). If you don't use it, leave out --preset other") {
		t.Fatalf("Gather = %v", err)
	}
}

// The holographic preset backs up memory_store.db in the Hermes folder and
// in each profile that has one, and nothing else, leaving its -wal and -shm
// files to the safe copy. HERMES_HOME names another Hermes folder. Given
// with the hermes preset, each file is backed up once, and the hermes preset
// alone never backs up memory_store.db. Finding none is an error that
// suggests leaving the preset out.
func TestGatherHolographic(t *testing.T) {
	presets, err := GetAll([]string{"hermes", "holographic"})
	if err != nil {
		t.Fatal(err)
	}
	hermes, holographic := presets[0], presets[1]
	home, err := filepath.EvalSymlinks(t.TempDir()) // databases are read from real paths
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".hermes")
	for _, rel := range []string{"memory_store.db", "profiles/coder/memory_store.db", "state.db"} {
		write(t, filepath.Join(dir, rel), sqliteFile)
	}
	for _, rel := range []string{
		"memory_store.db-wal", "memory_store.db-shm", "memory_store.db.bak", "config.yaml", "memories/MEMORY.md",
		"logs/agent.log", "profiles/coder/memory_store.db-wal", "profiles/writer/SOUL.md", "profiles/writer/data/memory_store.db",
	} {
		write(t, filepath.Join(dir, rel), "left out")
	}
	gather := func(e Env, presets ...*Preset) (*Found, error) {
		return e.Gather(presets, t.TempDir(), plain)
	}

	f, err := gather(envOf(home, nil), holographic)
	if err != nil {
		t.Fatal(err)
	}
	files, dbs := rels(f)
	if want := []string{"hermes/memory_store.db", "hermes/profiles/coder/memory_store.db"}; files != nil || !slices.Equal(dbs, want) {
		t.Fatalf("files %v, databases %v, want only databases %v", files, dbs, want)
	}

	f, err = gather(envOf(home, nil), hermes)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hermes/config.yaml", "hermes/memories/MEMORY.md", "hermes/profiles/writer/SOUL.md", "hermes/state.db"}
	if got := slices.Sorted(slices.Values(f.Paths())); !slices.Equal(got, want) {
		t.Fatalf("the hermes preset backed up %v, want %v", got, want)
	}
	want = slices.Sorted(slices.Values(slices.Concat(want, []string{"hermes/memory_store.db", "hermes/profiles/coder/memory_store.db"})))
	f, err = gather(envOf(home, nil), hermes, holographic)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(f.Paths())); !slices.Equal(got, want) {
		t.Fatalf("hermes and holographic backed up %v, want %v", got, want)
	}

	other := filepath.Join(home, "elsewhere")
	write(t, filepath.Join(other, "memory_store.db"), sqliteFile)
	f, err = gather(envOf(home, map[string]string{"HERMES_HOME": other}), holographic)
	if err != nil {
		t.Fatal(err)
	}
	if _, dbs := rels(f); !slices.Equal(dbs, []string{"hermes/memory_store.db"}) || !strings.HasPrefix(f.Databases[0].String(), other) {
		t.Fatalf("with HERMES_HOME: databases %v from %s", dbs, f.Databases[0])
	}

	empty := t.TempDir()
	write(t, filepath.Join(empty, "SOUL.md"), "a Hermes without Holographic memory")
	_, err = gather(envOf(home, map[string]string{"HERMES_HOME": empty}), hermes, holographic)
	if want := "found nothing to back up for the holographic preset. It looks in " + filepath.Join(empty, "memory_store.db") + ", " +
		filepath.Join(empty, "profiles", "*", "memory_store.db") + ". If you don't use it, leave out --preset holographic"; !errors.Is(err, ErrNothing) || err.Error() != want {
		t.Fatalf("Gather = %v, want %s", err, want)
	}
}

// A database a preset names is backed up under its path when its
// connection's variable is set, or with its default when it is not, and is
// skipped without either. Its password is never shown. A connection two
// presets name is backed up once, under the first preset's path, taking
// presets in name order, and counts for both.
func TestGatherDatabases(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	mk := func(name, from, to string) *Preset {
		t.Helper()
		p, err := Parse(name, []byte(`{"name": "`+name+`", "databases": [{"kind": "postgres", "from": "`+from+`", "to": "`+to+`"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	gather := func(vars map[string]string, presets ...*Preset) (*Found, error) {
		return envOf(t.TempDir(), vars).Gather(presets, t.TempDir(), plain)
	}
	withDefault := mk("d", "${D_URL:-postgresql://u:s3cret@localhost/def}", "d/def.sql")
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{nil, "postgresql://u@localhost/def"},
		{map[string]string{"D_URL": ""}, "postgresql://u@localhost/def"},
		{map[string]string{"D_URL": "postgresql+psycopg://a:s3cret@h:5432/mem"}, "postgresql://a@h:5432/mem"},
	} {
		f, err := gather(c.vars, withDefault)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Databases) != 1 || f.Databases[0].Name() != "d/def.sql" || f.Databases[0].String() != c.want || f.Files != nil {
			t.Fatalf("%v: databases %v", c.vars, f.Databases)
		}
		if p := f.Paths(); !slices.Equal(p, []string{"d/def.sql"}) {
			t.Fatalf("%v: paths %v", c.vars, p)
		}
	}

	// Without the variable or a default, nothing is found, and the message
	// names each variable that is not set, for paths and databases alike,
	// in the order the preset lists them.
	if _, err := gather(nil, mk("v", "${V_URL}", "v.sql")); !errors.Is(err, ErrNothing) || !strings.HasSuffix(err.Error(), "for the v preset. It looks in the database in $V_URL (not set)") {
		t.Fatalf("unset: %v", err)
	}
	both, err := Parse("m", []byte(`{"name": "m", "paths": [{"from": "${M_DIR}/data/${M_SUB:-x}", "to": "m"}, {"from": "/m", "to": "m2"}],
		"databases": [{"kind": "postgres", "from": "postgresql://${M_USER}@h:${M_PORT:-5432}/${M_DB}?user=${M_USER}", "to": "m.sql"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gather(nil, both); err == nil || !strings.HasSuffix(err.Error(), "It looks in $M_DIR (not set), /m, the database in $M_USER and $M_DB (not set)") {
		t.Fatalf("unset path and database: %v", err)
	}

	// Errors name the variables the connection came from, or the preset's
	// default, and never the password.
	for _, c := range []struct {
		p    *Preset
		vars map[string]string
		want string
	}{
		{withDefault, map[string]string{"D_URL": "postgresql://a:s3cret@h"}, "the connection in D_URL, used by the d preset, names no database"},
		{mk("two", "postgresql://${USER_X}:s3cret@${HOST_X}", "t.sql"), map[string]string{"USER_X": "a", "HOST_X": "h"}, "the connection in USER_X and HOST_X, used by the two preset, names no database"},
		{mk("same", "postgresql://${H_X}:s3cret@${H_X}", "s.sql"), map[string]string{"H_X": "h"}, "the connection in H_X, used by the same preset, names no database"},
		{mk("bad", "${B_URL:-mysql://u:s3cret@h/db}", "b.sql"), map[string]string{"OTHER": "x"}, "the bad preset's default database connection is not a Postgres connection"},
		{mk("lit", "mysql://u:s3cret@h/db", "l.sql"), nil, "the lit preset's database connection is not a Postgres connection"},
		{mk("mix", "postgresql://${U_X}:s3cret@${H_X:-h}", "x.sql"), map[string]string{"U_X": "u"}, "the connection in U_X, used by the mix preset, names no database"},
	} {
		_, err := gather(c.vars, c.p)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v, want %q", c.p.Name, err, c.want)
		}
	}

	// A copy that fails says where the connection came from, and the
	// database's option is its preset.
	for _, c := range []struct {
		p     *Preset
		vars  map[string]string
		about string
	}{
		{withDefault, nil, "This is the d preset's default connection, used when D_URL is not set. If the database is elsewhere, set D_URL, in the cron line too"},
		{withDefault, map[string]string{"D_URL": ""}, "This is the d preset's default connection, used when D_URL is not set. If the database is elsewhere, set D_URL, in the cron line too"},
		{withDefault, map[string]string{"D_URL": "postgresql://h/mem"}, "The d preset read this connection from D_URL"},
		{mk("mix", "postgresql://${U_X}@${H_X:-h}/db", "x.sql"), map[string]string{"U_X": "u"}, "The mix preset read this connection from U_X, with its default for H_X, which is not set. If the database is elsewhere, set H_X, in the cron line too"},
		{mk("mix", "postgresql://${U_X}@${H_X:-h}:${P_X:-5432}/db", "x.sql"), map[string]string{"U_X": "u"}, "The mix preset read this connection from U_X, with its defaults for H_X and P_X, which are not set. If the database is elsewhere, set H_X and P_X, in the cron line too"},
		{mk("a", "postgresql://${A_U:-u}@${A_H:-h}/db", "a.sql"), nil, "This is the a preset's default connection, used when A_U and A_H are not set. If the database is elsewhere, set A_U and A_H, in the cron line too"},
		{mk("lit", "postgresql://h/db", "l.sql"), nil, ""},
	} {
		f, err := gather(c.vars, c.p)
		if err != nil {
			t.Fatal(err)
		}
		db, ok := f.Databases[0].(presetDB)
		if !ok || db.about != c.about || db.Flag() != "--preset "+c.p.Name {
			t.Errorf("%s %v: %#v, want about %q", c.p.Name, c.vars, f.Databases[0], c.about)
		}
	}

	// A preset built in code, never read by Parse, with a kind a preset
	// cannot name, is an error rather than a crash.
	code := &Preset{Name: "code", Databases: []Database{{Kind: "sqlite", From: "/a.db", To: "a.db"}}}
	if _, err := gather(nil, code); err == nil || err.Error() != `preset code: "sqlite" is not a kind of database a preset can name. A database file is found by its path` {
		t.Fatalf("a kind no preset can name: %v", err)
	}

	// Taken in name order, b's path wins over c's, whatever order they are
	// given in, and both presets count as finding it.
	same := "${SAME:-postgresql://u:pw1@h/mem}"
	b, c := mk("b", same, "b/mem.sql"), mk("c", "postgresql://u:pw2@h/mem", "c/mem.sql")
	f, err := gather(nil, c, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Databases) != 1 || f.Databases[0].Name() != "b/mem.sql" {
		t.Fatalf("databases %v", f.Databases)
	}
	if err := f.CheckGone(); err != nil {
		t.Fatal(err)
	}
	f.Drop([]string{"b/mem.sql"})
	if err := f.CheckGone(); !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "the b preset") {
		t.Fatalf("CheckGone after Drop = %v", err)
	}
}

// failingDB is a database whose copy fails with err.
type failingDB struct{ err error }

func (failingDB) Name() string   { return "memory.sql" }
func (failingDB) String() string { return "postgresql://h/memory" }
func (failingDB) Flag() string   { return "--postgres" }
func (d failingDB) Copy(context.Context, source.CopyOptions) (source.Meta, error) {
	return source.Meta{Mode: 0o600}, d.err
}

// When a preset database's program fails, its error says where the
// connection came from, and is still the program's error. It says nothing
// more when the copy works, is stopped or cannot start the program, or when
// the connection holds no variables.
func TestPresetDBCopy(t *testing.T) {
	about := "The t preset read this connection from T_URL"
	refused := &proc.Error{Program: "pg_dump", Err: errors.New("exit status 1"), Stderr: "pg_dump: error: connection refused"}
	running := &proc.Error{Program: "pg_dump", Err: errors.New("exit status 1"), Stderr: "Is the server running on that host?"}
	silent := &proc.Error{Program: "pg_dump", Err: errors.New("exit status 1")}
	missing := fmt.Errorf("salt needs the pg_dump program, which %w", proc.ErrMissingProgram)
	for _, c := range []struct {
		err   error
		about string
		want  string
	}{
		{refused, about, refused.Error() + ". " + about},
		{running, about, running.Error() + " " + about},
		{silent, about, "pg_dump: exit status 1. " + about},
		{refused, "", refused.Error()},
		{context.Canceled, about, context.Canceled.Error()},
		{missing, about, missing.Error()},
	} {
		meta, err := presetDB{Database: failingDB{c.err}, preset: "t", about: c.about}.Copy(context.Background(), source.CopyOptions{})
		var failed *proc.Error
		if err == nil || err.Error() != c.want || !errors.Is(err, c.err) || errors.As(c.err, &failed) != errors.As(err, &failed) || meta.Mode != 0o600 {
			t.Errorf("Copy = %v, %v, want %q", meta, err, c.want)
		}
	}
	if _, err := (presetDB{Database: failingDB{}, about: about}).Copy(context.Background(), source.CopyOptions{}); err != nil {
		t.Errorf("a copy that works: %v", err)
	}
}

// The honcho preset backs up Honcho's database, with Honcho's own default
// connection when DB_CONNECTION_URI is not set, and Honcho's settings on
// their own, in Hermes and in each Hermes profile. A settings file holding
// an API key or a token is left out. HONCHO_CONFIG_DIR and HERMES_HOME name
// other folders. Given with hermes, it adds only its own files.
func TestGatherHoncho(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	presets, err := GetAll([]string{"hermes", "honcho"})
	if err != nil {
		t.Fatal(err)
	}
	hermes, honcho := presets[0], presets[1]
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".hermes")
	write(t, filepath.Join(home, ".honcho", "config.json"), `{"environmentUrl": "http://localhost:8000"}`)
	write(t, filepath.Join(home, ".honcho", "profiles", "local", "config.toml"), "[llm]\nOPENAI_API_KEY = \"sk-1\"\n")
	write(t, filepath.Join(dir, "honcho.json"), `{"hosts": {"hermes": {"workspace": "w", "oauth": {"refreshToken": "rt-1"}}}}`)
	write(t, filepath.Join(dir, "profiles", "coder", "honcho.json"), `{"apiKey": "hch-1", "workspace": "w"}`)
	write(t, filepath.Join(dir, "profiles", "writer", "honcho.json"), `{"workspace": "w", "contextTokens": 800}`)
	write(t, filepath.Join(dir, "SOUL.md"), "soul")
	gather := func(vars map[string]string, presets ...*Preset) *Found {
		t.Helper()
		f, err := envOf(home, vars).Gather(presets, t.TempDir(), plain)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	f := gather(nil, honcho)
	files, dbs := rels(f)
	if want := []string{"hermes/profiles/writer/honcho.json", "honcho/config.json"}; !slices.Equal(files, want) || !slices.Equal(dbs, []string{"honcho/honcho.sql"}) {
		t.Fatalf("files %v, databases %v", files, dbs)
	}
	if got := f.Databases[0].String(); got != "postgresql://postgres@localhost:5432/postgres" {
		t.Fatalf("default connection %s", got)
	}
	var left []string
	for _, l := range f.LeftOut {
		left = append(left, strings.TrimPrefix(l.Path, dir+string(filepath.Separator))+": "+l.Why)
	}
	slices.Sort(left)
	if want := []string{
		"honcho.json: its setting refreshToken holds a secret",
		filepath.Join("profiles", "coder", "honcho.json") + ": its setting apiKey holds a secret",
	}; !slices.Equal(left, want) {
		t.Fatalf("left out %v, want %v", left, want)
	}

	other, hermesHome := filepath.Join(home, "honcho-config"), filepath.Join(home, "hermes-home")
	write(t, filepath.Join(other, "config.json"), `{"environmentUrl": "http://localhost:8001"}`)
	write(t, filepath.Join(hermesHome, "honcho.json"), `{"workspace": "w"}`)
	f = gather(map[string]string{
		"DB_CONNECTION_URI": "postgresql+psycopg://honcho:s3cret@db.internal:6543/memory",
		"HONCHO_CONFIG_DIR": other,
		"HERMES_HOME":       hermesHome,
	}, honcho)
	files, dbs = rels(f)
	if !slices.Equal(files, []string{"hermes/honcho.json", "honcho/config.json"}) || !slices.Equal(dbs, []string{"honcho/honcho.sql"}) {
		t.Fatalf("with variables: files %v, databases %v", files, dbs)
	}
	if got := f.Databases[0].String(); got != "postgresql://honcho@db.internal:6543/memory" {
		t.Fatalf("with DB_CONNECTION_URI: %s", got)
	}
	for _, x := range f.Files {
		if want := map[string]string{"hermes/honcho.json": hermesHome, "honcho/config.json": other}[x.Rel]; filepath.Dir(x.Path) != want {
			t.Errorf("%s came from %s, want %s", x.Rel, x.Path, want)
		}
	}

	f = gather(nil, hermes, honcho)
	want := []string{"hermes/SOUL.md", "hermes/profiles/writer/honcho.json", "honcho/config.json", "honcho/honcho.sql"}
	if got := slices.Sorted(slices.Values(f.Paths())); !slices.Equal(got, want) {
		t.Fatalf("hermes and honcho backed up %v, want %v", got, want)
	}
}

func TestGatherUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	home := t.TempDir()
	sub := mkdir(t, home, "tool", "sub")
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	os.Chmod(sub, 0)
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain); err == nil {
		t.Fatal("Gather read an unreadable folder")
	}
	os.Chmod(sub, 0o755)
	secret := filepath.Join(home, "tool", "settings.yaml")
	write(t, secret, "a: b")
	os.Chmod(secret, 0)
	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain)
	if err != nil || len(f.LeftOut) != 1 || !strings.Contains(f.LeftOut[0].Why, "could not be read to check it for secrets") {
		t.Fatalf("Gather = %+v, %v", f, err)
	}
	os.Chmod(secret, 0o644)
	db := filepath.Join(home, "tool", "x.db")
	write(t, db, sqliteFile)
	os.Chmod(db, 0)
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain); err == nil {
		t.Fatal("Gather read an unreadable file")
	}
}

func TestSecretIn(t *testing.T) {
	keys := []string{"*api_key", "token"}
	dir := t.TempDir()
	for content, want := range map[string]string{
		"level: 3\n":                              "",
		"api_key: \"\"\n":                         "",
		"api_key: null\n":                         "",
		"api_key: ~\n":                            "",
		"api_key:\n":                              "",
		"api_key: []\n":                           "",
		"tokens: 5\nmax_token: 3\n":               "",
		"# api_key: sk-old\n":                     "",
		"llm_api_key: sk-1\n":                     "its setting llm_api_key holds a secret",
		"LLM_API_KEY: sk-1\n":                     "its setting LLM_API_KEY holds a secret",
		"memory:\n  provider:\n    token: abc\n":  "its setting token holds a secret",
		"list:\n  - token: abc\n":                 "its setting token holds a secret",
		"api_key:\n  sk-on-the-next-line\n":       "its setting api_key holds a secret",
		"api_key: |\n  sk-block\n":                "its setting api_key holds a secret",
		"api_key: [sk-1]\n":                       "its setting api_key holds a secret",
		"max_token: 512\n":                        "",
		"token: false\n":                          "",
		"token: 0x1F\n":                           "",
		"token:\n  enabled: true\n":               "",
		"token:\n  value: abc\n":                  "its setting token holds a secret",
		"token: \"512\"\n":                        "its setting token holds a secret",
		"token: !!binary c2stMQ==\n":              "its setting token holds a secret",
		"base: &b sk-1\napi_key: *b\n":            "its setting api_key holds a secret",
		"a: 1\n---\ntoken: abc\n":                 "its setting token holds a secret",
		`{"token": "abc"}`:                        "its setting token holds a secret",
		"api_key: [unclosed\n":                    "it could not be read as YAML to check it for secrets",
		strings.Repeat("x", maxSecretsFile+1):     "it is too large to check for secrets",
		strings.Repeat("a: 1\n", 10) + "\t- bad:": "it could not be read as YAML to check it for secrets",
		// Only ${NAME} names where the secret is kept. Anything more may
		// hold one, as a default after :- can.
		"api_key: ${MODEL_API_KEY}\n":  "",
		"api_key: \"${_K2}\"\n":        "",
		"token:\n  - ${A}\n  - ${B}\n": "",
		"api_key: sk-${X}\n":           "its setting api_key holds a secret",
		"api_key: ${X}-sk\n":           "its setting api_key holds a secret",
		"api_key: ${X:-sk-1}\n":        "its setting api_key holds a secret",
		"api_key: ${X}${Y}\n":          "its setting api_key holds a secret",
		"api_key: $X\n":                "its setting api_key holds a secret",
		"api_key: ${1X}\n":             "its setting api_key holds a secret",
		"api_key: \" ${X}\"\n":         "its setting api_key holds a secret",
		"api_key: |\n  ${X}\n":         "its setting api_key holds a secret",
		"api_key: !!binary JHtYfQ==\n": "its setting api_key holds a secret",
		"token:\n  - ${A}\n  - sk-1\n": "its setting token holds a secret",
		"api_key: ${env:MODEL_KEY}\n":  "",
		"api_key: \"${env:_K2}\"\n":    "",
		"api_key: ${env:}\n":           "its setting api_key holds a secret",
		"api_key: \"${env: K}\"\n":     "its setting api_key holds a secret",
		"api_key: ${ K }\n":            "its setting api_key holds a secret",
		"api_key: ${ENV:K}\n":          "its setting api_key holds a secret",
		"api_key: ${env:K:-sk-1}\n":    "its setting api_key holds a secret",
		"api_key: ${env:env:K}\n":      "its setting api_key holds a secret",
		"api_key: ${bitwarden:K}\n":    "its setting api_key holds a secret",
		"api_key: ${vault:a/b}\n":      "its setting api_key holds a secret",
	} {
		p := filepath.Join(dir, "f.yaml")
		write(t, p, content)
		if got, secret, _ := secretIn(p, keys); got != want || secret != strings.HasSuffix(want, "holds a secret") {
			t.Errorf("secretIn(%.40q) = %q, want %q", content, got, want)
		}
	}
	// A setting named like a secret that holds a number and no text is not
	// taken for a secret, since secrets almost always mix letters and
	// digits, but is named so the person can be told.
	for content, want := range map[string]string{
		"token: 123456\n":                "token",
		"token: 0x1F\n":                  "token",
		"token: 1.5\n":                   "token",
		"token:\n  pin: 1234\n":          "token",
		"a: 1\n---\ntoken: 7\n":          "token",
		"llm_api_key: 1\ntoken: 2\n":     "llm_api_key",
		"token: false\n":                 "",
		"token: null\n":                  "",
		"tokens: 5\n":                    "",
		"token: 5\napi_key: sk-1\n":      "",
		"token:\n  1: abc\n":             "",
		"token:\n  pin: 1\n  key: abc\n": "",
	} {
		p := filepath.Join(dir, "f.yaml")
		write(t, p, content)
		if _, _, got := secretIn(p, keys); got != want {
			t.Errorf("secretIn(%q) names the number in %q, want %q", content, got, want)
		}
	}
	// A file deleted since it was listed is for the caller to skip.
	if got, secret, _ := secretIn(filepath.Join(dir, "missing"), keys); got != "" || secret {
		t.Errorf("missing file: %q", got)
	}
	if got, _, _ := secretIn(dir, keys); !strings.Contains(got, "could not be read") {
		t.Errorf("folder: %q", got)
	}
}

// IsSQLite is what tells databases apart from other files.
func TestDatabasesAreSQLite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	write(t, p, sqliteFile)
	if ok, err := source.IsSQLite(p); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

// Files are read from where a place really is, the path checked against the
// repo, even when it is found through a symlink, so a symlink changed while
// salt backs up cannot lead the reads elsewhere. Messages still show the
// path the preset names.
func TestGatherReadsRealPaths(t *testing.T) {
	home := t.TempDir()
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "real", "a.md"), "a")
	write(t, filepath.Join(home, "real", "m.db"), sqliteFile)
	write(t, filepath.Join(home, "real", "settings.yaml"), "token: abc")
	os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "tool"))
	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Files) != 1 || f.Files[0].Path != filepath.Join(real, "real", "a.md") {
		t.Errorf("files %+v", f.Files)
	}
	if len(f.Databases) != 1 || f.Databases[0].String() != filepath.Join(real, "real", "m.db") {
		t.Errorf("databases %v", f.Databases)
	}
	if len(f.LeftOut) != 1 || f.LeftOut[0].Path != filepath.Join(home, "tool", "settings.yaml") {
		t.Errorf("left out %+v", f.LeftOut)
	}
}

// A file found through a symlink is checked for secrets by either name.
func TestGatherChecksSecretsThroughSymlinks(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "elsewhere", "real.yml"), "api_key: sk-1")
	mkdir(t, home, "tool")
	os.Symlink(filepath.Join(home, "elsewhere", "real.yml"), filepath.Join(home, "tool", "settings.yaml"))
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	p, err := Parse("t", []byte(`{"name": "t", "paths": [{"from": "~/tool/settings.yaml", "to": "settings.yaml"}, {"from": "~/tool/a.md", "to": "a.md"}],
		"secrets": [{"files": ["settings.yaml"], "keys": ["api_key"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f, err := envOf(home, nil).Gather([]*Preset{p}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"a.md"}) || len(f.LeftOut) != 1 || f.LeftOut[0].Path != filepath.Join(home, "tool", "settings.yaml") {
		t.Fatalf("files %v, left out %+v", files, f.LeftOut)
	}
}

// A -wal file beside a database is never backed up on its own, even when
// the database itself is skipped, since without it the -wal is useless.
func TestGatherSidecarOfSkippedDatabase(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "tool", "old.db"), sqliteFile)
	write(t, filepath.Join(home, "tool", "old.db-wal"), "wal")
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	p, err := Parse("t", []byte(`{"name": "t", "paths": [{"from": "~/tool", "to": "tool"}], "skip": ["old.db"]}`))
	if err != nil {
		t.Fatal(err)
	}
	f, err := envOf(home, nil).Gather([]*Preset{p}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, dbs := rels(f); !slices.Equal(files, []string{"tool/a.md"}) || len(dbs) != 0 {
		t.Fatalf("files %v, databases %v", files, dbs)
	}
}

// A file or folder deleted after the walk listed it, as a tool's files can
// be at any time, is skipped, and the files Gather adds are marked live. A
// preset whose only place is deleted while it is walked has found nothing.
func TestGatherSkipsWhatVanishes(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir()) // the walk sees real paths
	if err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(home, "tool")
	write(t, filepath.Join(tool, "kept.md"), "kept")
	write(t, filepath.Join(tool, "gone.md"), "gone")
	write(t, filepath.Join(tool, "gone.db"), sqliteFile)
	write(t, filepath.Join(tool, "settings.yaml"), "api_key: sk-1")
	write(t, filepath.Join(tool, "sub", "deep.md"), "deep")
	vanish := []string{"gone.md", "gone.db", "settings.yaml", "sub"}
	listedHook = func(p string) {
		if rel, err := filepath.Rel(tool, p); err == nil && slices.Contains(vanish, rel) {
			os.RemoveAll(p)
		}
	}
	t.Cleanup(func() { listedHook = nil })
	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain)
	if err != nil {
		t.Fatal(err)
	}
	files, dbs := rels(f)
	if !slices.Equal(files, []string{"tool/kept.md"}) || len(dbs) != 0 || len(f.LeftOut) != 0 {
		t.Fatalf("files %v, databases %v, left out %+v", files, dbs, f.LeftOut)
	}
	if !f.Files[0].Live {
		t.Fatal("a gathered file is not marked live")
	}
	if !slices.Equal(f.Places, []string{"tool"}) {
		t.Fatalf("places = %v", f.Places)
	}

	vanish = []string{"."}
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain); !errors.Is(err, ErrNothing) {
		t.Fatalf("Gather = %v", err)
	}
}

// A place deleted after it was found, a folder or a single file, is skipped
// and left out of Places, so the backup goes on and names it as missing. A
// place found under two paths leaves both out.
func TestGatherSkipsVanishedPlaces(t *testing.T) {
	for _, gone := range []string{"data", "single.txt"} {
		t.Run(gone, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir()) // the walk sees real paths
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(home, "data", "a.md"), "a")
			write(t, filepath.Join(home, "single.txt"), "one file")
			p, err := Parse("t", []byte(`{"name": "t", "paths": [
				{"from": "~/data", "to": "first"},
				{"from": "${DATA}", "to": "second"},
				{"from": "~/single.txt", "to": "single.txt"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			listedHook = func(p string) {
				if p == filepath.Join(home, gone) {
					os.RemoveAll(p)
				}
			}
			t.Cleanup(func() { listedHook = nil })
			f, err := envOf(home, map[string]string{"DATA": filepath.Join(home, "data")}).Gather([]*Preset{p}, t.TempDir(), plain)
			if err != nil {
				t.Fatal(err)
			}
			wantPlaces, wantFiles := []string{"single.txt"}, []string{"single.txt"}
			if gone == "single.txt" {
				wantPlaces, wantFiles = []string{"first", "second"}, []string{"first/a.md"}
			}
			if !slices.Equal(f.Places, wantPlaces) {
				t.Errorf("places = %v, want %v", f.Places, wantPlaces)
			}
			if files, _ := rels(f); !slices.Equal(files, wantFiles) {
				t.Errorf("files = %v, want %v", files, wantFiles)
			}
		})
	}
}

// Two presets that share only a place deleted before it is read have both
// found nothing, and the first in name order is named.
func TestGatherSharedPlaceVanishes(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir()) // the walk sees real paths
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "data", "a.md"), "a")
	var presets []*Preset
	for _, name := range []string{"b", "a"} {
		p, err := Parse(name, []byte(`{"name": "`+name+`", "paths": [{"from": "~/data", "to": "data"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		presets = append(presets, p)
	}
	listedHook = func(p string) {
		if p == filepath.Join(home, "data") {
			os.RemoveAll(p)
		}
	}
	t.Cleanup(func() { listedHook = nil })
	_, err = envOf(home, nil).Gather(presets, t.TempDir(), plain)
	if !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "for the a preset") {
		t.Fatalf("Gather = %v", err)
	}
}

// A spot deleted between Find and its walk is skipped and reported gone. Any
// other error on it still stops the walk.
func TestWalkSkipsGoneSpot(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "missing")
	s := &spot{place: Place{Abs: missing, Rel: "a", Real: missing}, real: missing}
	w := &walker{f: &Found{by: map[*Preset][]string{}}, spots: map[string]*spot{missing: s}, show: plain}
	gone, err := w.walk(s, []*Preset{testPreset(t)})
	if err != nil || !gone || len(w.f.Files) != 0 || len(w.f.by) != 0 {
		t.Fatalf("walk = %v, %v, files %v, by %v", gone, err, w.f.Files, w.f.by)
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	locked := mkdir(t, home, "locked")
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	s = &spot{place: Place{Abs: locked, Rel: "b", Real: locked}, real: locked}
	if gone, err := w.walk(s, []*Preset{testPreset(t)}); gone || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("walk = %v, %v", gone, err)
	}
}

// Drop removes the paths gone from the files, databases and places found,
// and from what each preset backs up. CheckGone then fails for a preset left
// with nothing, naming the first in name order, and passes while each has
// something left.
func TestDropAndCheckGone(t *testing.T) {
	db, err := source.NewSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := &Preset{Name: "a"}, &Preset{Name: "b"}
	found := func() *Found {
		return &Found{
			Files:     []seal.Extra{{Rel: "one.md"}, {Rel: "dir/two.md"}},
			Databases: []source.Database{db},
			Places:    []string{"one.md", "dir", "x.db"},
			by:        map[*Preset][]string{a: {"one.md", "x.db"}, b: {"x.db"}},
		}
	}
	f := found()
	f.Drop(nil)
	if err := f.CheckGone(); err != nil || len(f.Files) != 2 || len(f.Databases) != 1 || len(f.Places) != 3 {
		t.Fatalf("nothing gone: %v, %+v", err, f)
	}
	f = found()
	f.Drop([]string{"one.md", "dir/two.md"})
	if err := f.CheckGone(); err != nil {
		t.Fatalf("files gone: %v", err)
	}
	if len(f.Files) != 0 || len(f.Databases) != 1 || !slices.Equal(f.Places, []string{"dir", "x.db"}) {
		t.Fatalf("files gone: %+v", f)
	}
	f = found()
	f.Drop([]string{"x.db"})
	if err := f.CheckGone(); !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "for the b preset") || len(f.Databases) != 0 {
		t.Fatalf("database gone: %v, %+v", err, f)
	}
	f = found()
	f.Drop([]string{"x.db", "one.md"})
	if err := f.CheckGone(); !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "for the a preset") {
		t.Fatalf("both gone: %v", err)
	}
}
