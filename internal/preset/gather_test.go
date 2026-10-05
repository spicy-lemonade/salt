package preset

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "for the t preset. It looks in <tool>, <tool>, <single.txt>") {
		t.Fatalf("Gather = %v", err)
	}
	// The second preset is checked too.
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	other, _ := Parse("other", []byte(`{"name": "other", "paths": [{"from": "${UNSET}", "to": "o"}]}`))
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t), other}, t.TempDir(), plain); !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "other preset") {
		t.Fatalf("Gather = %v", err)
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
// place deleted while it is walked is still an error.
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

	vanish = []string{"."}
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, t.TempDir(), plain); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Gather = %v", err)
	}
}
