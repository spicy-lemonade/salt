package preset

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	home := t.TempDir()
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
	write(t, filepath.Join(tool, "settings.yaml"), "level: 3\napi_key: \"\"\n")
	write(t, filepath.Join(home, "profiles", "work", "tool", "settings.yaml"), "llm:\n  api_key: sk-123\n")
	write(t, filepath.Join(home, "profiles", "work", "tool", "bank", "memory.db"), sqliteFile)
	write(t, filepath.Join(home, "single.txt"), "one file")
	os.Symlink("notes.md", filepath.Join(tool, "link.md"))
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(tool, "notes.md"), when, when)

	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, plain)
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
	// Each file keeps its own permissions and last-modified date.
	for _, x := range f.Files {
		if x.Rel == "tool/notes.md" && (!x.ModTime.Equal(when) || x.Mode.Perm() != 0o640) {
			t.Errorf("notes.md: %v %v", x.ModTime, x.Mode)
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
	if len(f.Roots) != 3 {
		t.Errorf("roots = %v", f.Roots)
	}
}

// A place found twice, as when a variable names a default folder, is backed
// up only under the first path, and so is a place inside one already found.
func TestGatherTakesEachPlaceOnce(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "data", "a.md"), "a")
	p, err := Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "~/data", "to": "first"},
		{"from": "${DATA}", "to": "second"},
		{"from": "~/data/a.md", "to": "third.md"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(filepath.Join(home, "data"), link)
	f, err := envOf(home, map[string]string{"DATA": link}).Gather([]*Preset{p}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"first/a.md"}) {
		t.Fatalf("files = %v", files)
	}
}

// A preset that finds nothing says where it looked.
func TestGatherFindsNothing(t *testing.T) {
	home := t.TempDir()
	mkdir(t, home, "tool") // an empty folder holds nothing to back up
	write(t, filepath.Join(home, "profiles", "a", "tool", "settings.yaml"), "token: abc")
	_, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, func(p string) string { return "<" + filepath.Base(p) + ">" })
	if !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "for the t preset. It looks in <tool>, <tool>, <single.txt>") {
		t.Fatalf("Gather = %v", err)
	}
	// The second preset is checked too.
	write(t, filepath.Join(home, "tool", "a.md"), "a")
	other, _ := Parse("other", []byte(`{"name": "other", "paths": [{"from": "${UNSET}", "to": "o"}]}`))
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t), other}, plain); !errors.Is(err, ErrNothing) || !strings.Contains(err.Error(), "other preset") {
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
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, plain); err == nil {
		t.Fatal("Gather read an unreadable folder")
	}
	os.Chmod(sub, 0o755)
	secret := filepath.Join(home, "tool", "settings.yaml")
	write(t, secret, "a: b")
	os.Chmod(secret, 0)
	f, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, plain)
	if err != nil || len(f.LeftOut) != 1 || !strings.Contains(f.LeftOut[0].Why, "could not be read to check it for secrets") {
		t.Fatalf("Gather = %+v, %v", f, err)
	}
	os.Chmod(secret, 0o644)
	db := filepath.Join(home, "tool", "x.db")
	write(t, db, sqliteFile)
	os.Chmod(db, 0)
	if _, err := envOf(home, nil).Gather([]*Preset{testPreset(t)}, plain); err == nil {
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
		"base: &b sk-1\napi_key: *b\n":            "its setting api_key holds a secret",
		"a: 1\n---\ntoken: abc\n":                 "its setting token holds a secret",
		`{"token": "abc"}`:                        "its setting token holds a secret",
		"api_key: [unclosed\n":                    "it could not be read as YAML to check it for secrets",
		strings.Repeat("x", maxSecretsFile+1):     "it is too large to check for secrets",
		strings.Repeat("a: 1\n", 10) + "\t- bad:": "it could not be read as YAML to check it for secrets",
	} {
		p := filepath.Join(dir, "f.yaml")
		write(t, p, content)
		if got := secretIn(p, keys); got != want {
			t.Errorf("secretIn(%.40q) = %q, want %q", content, got, want)
		}
	}
	if got := secretIn(filepath.Join(dir, "missing"), keys); !strings.Contains(got, "could not be read") {
		t.Errorf("missing file: %q", got)
	}
	if got := secretIn(dir, keys); !strings.Contains(got, "could not be read") {
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
	f, err := envOf(home, nil).Gather([]*Preset{p}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := rels(f); !slices.Equal(files, []string{"a.md"}) || len(f.LeftOut) != 1 || f.LeftOut[0].Path != filepath.Join(home, "tool", "settings.yaml") {
		t.Fatalf("files %v, left out %+v", files, f.LeftOut)
	}
}
