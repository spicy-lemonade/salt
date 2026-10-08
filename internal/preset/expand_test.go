package preset

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// envOf reads variables from vars only, never the real environment.
func envOf(home string, vars map[string]string) Env {
	return Env{Home: home, Getenv: func(k string) string { return vars[k] }}
}

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// vars names each variable a template reads once, as set, as unset with a
// default wherever it is read, or as unset and read at least once without
// one, which orders it by its first read without a default. An empty
// variable is unset, as in expand. Without Getenv, none is set.
func TestVars(t *testing.T) {
	e := envOf("/home/me", map[string]string{"A": "a", "C": ""})
	for _, c := range []struct {
		template                string
		set, defaulted, missing []string
	}{
		{"${A}/${B:-x}/${A}/${C}/${B}/${E:-y}/${E:-z}/$D", []string{"A"}, []string{"E"}, []string{"C", "B"}},
		{"${G}/${G:-x}", nil, nil, []string{"G"}},
		{"${C:-x}/${F:-}", nil, []string{"C", "F"}, nil},
		{"/plain", nil, nil, nil},
	} {
		set, defaulted, missing := e.vars(c.template)
		if !slices.Equal(set, c.set) || !slices.Equal(defaulted, c.defaulted) || !slices.Equal(missing, c.missing) {
			t.Errorf("vars(%s) = %v, %v, %v, want %v, %v, %v", c.template, set, defaulted, missing, c.set, c.defaulted, c.missing)
		}
	}
	if set, defaulted, missing := (Env{}).vars("${A}/${B:-x}"); set != nil || !slices.Equal(defaulted, []string{"B"}) || !slices.Equal(missing, []string{"A"}) {
		t.Fatalf("without Getenv: %v, %v, %v", set, defaulted, missing)
	}
}

func TestExpand(t *testing.T) {
	e := envOf("/home/me", map[string]string{"SET": "/set", "EMPTY": ""})
	for template, want := range map[string]string{
		"${SET}/a":          "/set/a",
		"${SET:-/other}/a":  "/set/a",
		"${EMPTY:-/dflt}/a": "/dflt/a",
		"${UNSET:-~/x}":     "/home/me/x",
		"~":                 "/home/me",
		"~/a":               "/home/me/a",
		"/a/~b":             "/a/~b",
		"/plain":            "/plain",
	} {
		if got, ok := e.expand(template); !ok || got != want {
			t.Errorf("expand(%q) = %q, %v, want %q", template, got, ok, want)
		}
	}
	for _, template := range []string{"${UNSET}", "${EMPTY}/a", "/a/${UNSET}"} {
		if got, ok := e.expand(template); ok {
			t.Errorf("expand(%q) = %q, want skipped", template, got)
		}
	}
	// Without a home folder, ~ is skipped; without Getenv every variable
	// is unset.
	if got, ok := (Env{}).expand("~/a"); ok {
		t.Errorf("expand(~/a) without home = %q", got)
	}
	if got, ok := (Env{}).expand("${X:-/d}"); !ok || got != "/d" {
		t.Errorf("expand without Getenv = %q, %v", got, ok)
	}
}

// places returns what e.Find(x) finds, checking each place's real path and
// leaving it out, so tests can compare Abs and Rel alone.
func places(t *testing.T, e Env, x Path) []Place {
	t.Helper()
	got, err := e.Find(x)
	if err != nil {
		t.Fatal(err)
	}
	for i, pl := range got {
		if real, err := filepath.EvalSymlinks(pl.Abs); err != nil || pl.Real != real {
			t.Errorf("%s: real path %q, want %q (%v)", pl.Abs, pl.Real, real, err)
		}
		got[i].Real = ""
	}
	return got
}

func TestFindFixedPath(t *testing.T) {
	home := t.TempDir()
	data := mkdir(t, home, "tool", "data")
	e := envOf(home, nil)
	got := places(t, e, Path{From: "~/tool/data", To: "tool/data"})
	if !slices.Equal(got, []Place{{Abs: data, Rel: "tool/data"}}) {
		t.Fatalf("Find = %v", got)
	}
	// A missing path, or one whose variable is unset, finds nothing, and so
	// does a symlink to a missing path. A symlink is found as itself, with
	// where it leads as its real path.
	if got := places(t, e, Path{From: "~/tool/missing", To: "m"}); got != nil {
		t.Fatalf("missing: %v", got)
	}
	os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, "dangling"))
	if got := places(t, e, Path{From: "~/dangling", To: "d"}); got != nil {
		t.Fatalf("dangling symlink: %v", got)
	}
	os.Symlink(data, filepath.Join(home, "link"))
	if got := places(t, e, Path{From: "~/link", To: "l"}); !slices.Equal(got, []Place{{Abs: filepath.Join(home, "link"), Rel: "l"}}) {
		t.Fatalf("symlink: %v", got)
	}
	if got := places(t, e, Path{From: "${UNSET}", To: "u"}); got != nil {
		t.Fatalf("unset: %v", got)
	}
	// A relative path is taken from the current folder.
	t.Chdir(home)
	if got := places(t, e, Path{From: "tool/data", To: "d"}); len(got) != 1 || got[0].Abs != data {
		t.Fatalf("relative: %v", got)
	}
}

// Each * matches every folder there, leaving out hidden ones and files, and
// its name fills the * in the backup path.
func TestFindStars(t *testing.T) {
	home := t.TempDir()
	a := mkdir(t, home, "profiles", "a", "tool")
	b := mkdir(t, home, "profiles", "b", "tool")
	mkdir(t, home, "profiles", "c") // no tool folder
	mkdir(t, home, "profiles", ".hidden", "tool")
	os.WriteFile(filepath.Join(home, "profiles", "file"), nil, 0o644)
	got := places(t, envOf(home, nil), Path{From: "~/profiles/*/tool", To: "p/*/tool"})
	want := []Place{{Abs: a, Rel: "p/a/tool"}, {Abs: b, Rel: "p/b/tool"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Find = %v, want %v", got, want)
	}
	// Two stars, and a star at the end.
	x := mkdir(t, home, "w", "one", "s", "two")
	got = places(t, envOf(home, nil), Path{From: "~/w/*/s/*", To: "w/*/*"})
	if !slices.Equal(got, []Place{{Abs: x, Rel: "w/one/two"}}) {
		t.Fatalf("two stars: %v", got)
	}
	// A missing folder before the star finds nothing.
	if got := places(t, envOf(home, nil), Path{From: "~/none/*/tool", To: "n/*"}); got != nil {
		t.Fatalf("missing: %v", got)
	}
	// An unset variable after the star skips the path.
	if got := places(t, envOf(home, nil), Path{From: "~/profiles/*/${UNSET}", To: "n/*"}); got != nil {
		t.Fatalf("unset after star: %v", got)
	}
}

// A * with text around it in a part matches each folder named with that
// text and at least one character for the *, which is what fills the * in
// the backup path. Hidden folders are matched only by a part starting with
// a dot. A * that would fill a part with . or .. matches nothing.
func TestFindStarInPart(t *testing.T) {
	home := t.TempDir()
	work := mkdir(t, home, ".tool-work", "data")
	odd := mkdir(t, home, ".tool-a.b-c", "data")
	mkdir(t, home, ".tool-", "data")   // the * matches nothing
	mkdir(t, home, ".tool", "data")    // no dash
	mkdir(t, home, ".tool-..", "data") // the * would fill a part with ..
	mkdir(t, home, "tool-plain", "data")
	os.WriteFile(filepath.Join(home, ".tool-file"), nil, 0o644)
	got := places(t, envOf(home, nil), Path{From: "~/.tool-*/data", To: "profiles/*/data"})
	want := []Place{{Abs: odd, Rel: "profiles/a.b-c/data"}, {Abs: work, Rel: "profiles/work/data"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Find = %v, want %v", got, want)
	}

	// Text after the *, and the * kept inside a part of the backup path.
	ws := mkdir(t, home, "state", "ws-a.d")
	mkdir(t, home, "state", "ws-.d.x")
	got = places(t, envOf(home, nil), Path{From: "~/state/ws-*.d", To: "s/workspace-*"})
	if !slices.Equal(got, []Place{{Abs: ws, Rel: "s/workspace-a"}}) {
		t.Fatalf("text after the *: %v", got)
	}
	// A part that does not start with a dot leaves out hidden folders,
	// though the * could match the dot.
	mkdir(t, home, "state", ".x.d")
	got = places(t, envOf(home, nil), Path{From: "~/state/*.d", To: "s/*"})
	if !slices.Equal(got, []Place{{Abs: ws, Rel: "s/ws-a"}}) {
		t.Fatalf("hidden folder: %v", got)
	}
	// Such parts and whole * parts together.
	inner := mkdir(t, home, ".tool-work", "agents", "main", "skills")
	got = places(t, envOf(home, nil), Path{From: "~/.tool-*/agents/*/skills", To: "profiles/*/agents/*/skills"})
	if !slices.Equal(got, []Place{{Abs: inner, Rel: "profiles/work/agents/main/skills"}}) {
		t.Fatalf("two stars: %v", got)
	}
}

func TestMatchPart(t *testing.T) {
	for _, c := range []struct {
		pat, name, star string
		ok              bool
	}{
		{"*", "a", "a", true},
		{"*", ".", "", false},
		{"*", "..", "", false},
		{"ws-*", "ws-a", "a", true},
		{"ws-*", "ws-", "", false},
		{"ws-*", "w", "", false},
		{"ws-*", "x-ws-a", "", false},
		{"*.d", "a.d", "a", true},
		{"a*a", "a", "", false},
		{"a*a", "aba", "b", true},
		{"ws", "ws", "", false}, // no *
		{"ws", "wsx", "", false},
	} {
		if star, ok := matchPart(c.pat, c.name); star != c.star || ok != c.ok {
			t.Errorf("matchPart(%q, %q) = %q, %v, want %q, %v", c.pat, c.name, star, ok, c.star, c.ok)
		}
	}
}

// A * held by a variable is part of a folder's name, never matched.
func TestFindStarInVariable(t *testing.T) {
	home := t.TempDir()
	mkdir(t, home, "a", "x")
	star := mkdir(t, home, "a", "*")
	got := places(t, envOf(home, map[string]string{"DIR": filepath.Join(home, "a", "*")}), Path{From: "${DIR}", To: "d"})
	if !slices.Equal(got, []Place{{Abs: star, Rel: "d"}}) {
		t.Fatalf("Find = %v", got)
	}
}

func TestFindUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	home := t.TempDir()
	p := mkdir(t, home, "profiles")
	os.Chmod(p, 0)
	t.Cleanup(func() { os.Chmod(p, 0o755) })
	if _, err := envOf(home, nil).Find(Path{From: "~/profiles/*/tool", To: "p/*"}); err == nil {
		t.Fatal("Find read an unreadable folder")
	}
	if _, err := envOf(home, nil).Find(Path{From: "~/profiles/p-*", To: "p/*"}); err == nil {
		t.Fatal("Find read an unreadable folder for a * inside a part")
	}
	if _, err := envOf(home, nil).Find(Path{From: "~/profiles/x", To: "p"}); err == nil {
		t.Fatal("Find checked a path in an unreadable folder")
	}
}
