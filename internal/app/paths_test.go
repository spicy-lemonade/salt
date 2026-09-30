package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

func TestShortPath(t *testing.T) {
	sep := string(filepath.Separator)
	home := filepath.Join(sep, "Users", "name")
	for name, tc := range map[string]struct{ path, home, want string }{
		"inside home":          {filepath.Join(home, "backup"), home, "~" + sep + "backup"},
		"nested":               {filepath.Join(home, "a", "b"), home, "~" + sep + "a" + sep + "b"},
		"home itself":          {home, home, "~"},
		"home with trailing":   {filepath.Join(home, "backup"), home + sep, "~" + sep + "backup"},
		"outside home":         {filepath.Join(sep, "tmp", "x"), home, filepath.Join(sep, "tmp", "x")},
		"sibling with prefix":  {home + "2" + sep + "backup", home, home + "2" + sep + "backup"},
		"parent of home":       {filepath.Join(sep, "Users"), home, filepath.Join(sep, "Users")},
		"unknown home":         {filepath.Join(home, "backup"), "", filepath.Join(home, "backup")},
		"relative path":        {"backup", home, "backup"},
		"relative home":        {filepath.Join(home, "backup"), "name", filepath.Join(home, "backup")},
		"empty path":           {"", home, ""},
		"unclean path":         {filepath.Join(home, "a") + sep + ".." + sep + "b", home, "~" + sep + "b"},
		"dot-dot escapes home": {home + sep + ".." + sep + "other", home, home + sep + ".." + sep + "other"},
		"root as home":         {filepath.Join(sep, "tmp", "x"), sep, filepath.Join(sep, "tmp", "x")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ShortPath(tc.path, tc.home); got != tc.want {
				t.Fatalf("ShortPath(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
			}
		})
	}
}

// homeEnv is a test env whose home folder holds the backup repo, so the repo
// shows up as ~/backup.
func homeEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t)
	e.app.Home = filepath.Dir(e.root)
	return e
}

func TestInitShowsHomePathsButKeepsCommandsPastable(t *testing.T) {
	e := homeEnv(t)
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatalf("Init: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	if !strings.Contains(out, "salt is set up in ~/backup\n") {
		t.Errorf("summary does not show ~/backup:\n%s", out)
	}
	if !strings.Contains(out, "Pre-commit hook: ~/backup/.git/hooks/pre-commit") {
		t.Errorf("hook path not shortened:\n%s", out)
	}
	for _, cmd := range []string{
		"git -C " + `"` + e.root + `"` + " add .salt",
		"salt seal --prune \"$STAGE\" " + `"` + e.root + `"`,
		"salt restore " + `"` + e.root + `"`,
	} {
		if !strings.Contains(out, cmd) {
			t.Errorf("suggested command changed, want %q in:\n%s", cmd, out)
		}
	}
}

func TestInitShowsFullPathsOutsideHome(t *testing.T) {
	e := newEnv(t)
	e.app.Home = t.TempDir() // a different folder from the repo's
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "salt is set up in "+e.root+"\n") {
		t.Errorf("path outside home was changed:\n%s", e.ui.out.String())
	}
}

func TestInitErrorsUseHomePaths(t *testing.T) {
	e := homeEnv(t)
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	err := e.app.Init(InitOptions{Repo: e.root})
	if err == nil || !strings.HasPrefix(err.Error(), "~/backup is already set up") {
		t.Fatalf("second init: %v", err)
	}
	err = e.app.Init(InitOptions{Repo: filepath.Join(e.app.Home, "missing")})
	if err == nil || !strings.HasPrefix(err.Error(), "~/missing is not a git repository") {
		t.Fatalf("init outside a git repo: %v", err)
	}
}

func TestDoctorShowsHomePaths(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	e.ui.out.Reset()
	if err := e.app.Doctor(e.root); err != nil {
		t.Fatalf("Doctor: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	if !strings.Contains(out, "salt doctor: ~/backup\n") {
		t.Errorf("header not shortened:\n%s", out)
	}
	if !strings.Contains(out, "(~/backup/.git/hooks/pre-commit)") {
		t.Errorf("hook path not shortened:\n%s", out)
	}
	if !strings.Contains(out, "salt recovery test "+e.root+"` now") {
		t.Errorf("suggested command changed:\n%s", out)
	}

	e.ui.out.Reset()
	err := e.app.Doctor(filepath.Join(e.app.Home, "missing"))
	if !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), "~/missing is not a git repository") {
		t.Fatalf("doctor on a non-repo: %v\n%s", err, e.ui.out.String())
	}
}

func TestTrustMessagesShowHomePathsButKeepCommands(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	e.app.TrustDir = filepath.Join(t.TempDir(), "fresh")
	err := e.app.Seal(t.TempDir(), e.root, false)
	if !errors.Is(err, ErrNotTrusted) ||
		!strings.Contains(err.Error(), "the keys in ~/backup yet") ||
		!strings.Contains(err.Error(), "`salt trust "+e.root+"`") {
		t.Fatalf("seal on a new machine: %v", err)
	}

	e.ui.out.Reset()
	if err := e.app.Trust(e.root, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "Backups to ~/backup will be encrypted") {
		t.Errorf("trust header not shortened:\n%s", e.ui.out.String())
	}

	addAttackerKey(t, e.root)
	err = e.app.Seal(t.TempDir(), e.root, false)
	if !errors.Is(err, ErrNotTrusted) ||
		!strings.Contains(err.Error(), "keys or settings in ~/backup changed") ||
		!strings.Contains(err.Error(), "`salt trust "+e.root+"`") {
		t.Fatalf("seal after tampering: %v", err)
	}
}

func TestRestoreShowsHomePaths(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	e.ui.out.Reset()
	dest := filepath.Join(e.app.Home, "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.ui.out.String())
	}
	if !strings.Contains(e.ui.out.String(), "symlinks to ~/restored\n") {
		t.Errorf("destination not shortened:\n%s", e.ui.out.String())
	}

	e.ui.out.Reset()
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest, Force: true}); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "previous contents were moved to ~/restored") {
		t.Errorf("moved-aside path not shortened:\n%s", e.ui.out.String())
	}
}

func TestDoctorShowsKeyFileAsHomePath(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	r, _ := repo.Open(e.root)
	s, _ := e.store.Get(r.RecipientStrings[0])
	ls := locatedStore{&keys.MemStore{}, filepath.Join(e.app.Home, ".config", "salt", "keys", "k.json")}
	ls.Set(r.RecipientStrings[0], s)
	e.app.Store = ls
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "saved in private file ~/.config/salt/keys/k.json") {
		t.Errorf("key file not shortened:\n%s", e.ui.out.String())
	}
}

func TestForeignHookShowsHomePath(t *testing.T) {
	e := homeEnv(t)
	os.MkdirAll(filepath.Dir(e.hook), 0o755)
	os.WriteFile(e.hook, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	_, err := e.app.InstallHook(e.root)
	want := "a pre-commit hook already exists at ~/backup/.git/hooks/pre-commit; add `salt check` to it"
	if !errors.Is(err, hook.ErrForeign) || !strings.Contains(err.Error(), want) {
		t.Fatalf("InstallHook over a foreign hook: %v", err)
	}
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "Note: "+want) {
		t.Errorf("init note not shortened:\n%s", e.ui.out.String())
	}
}

func TestRestoreRefusalShowsHomePath(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	dest := filepath.Join(e.app.Home, "restored")
	os.MkdirAll(dest, 0o755)
	os.WriteFile(filepath.Join(dest, "x"), []byte("x"), 0o644)
	err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest})
	if err == nil || !strings.HasPrefix(err.Error(), "~/restored already exists and is not empty") {
		t.Fatalf("restore over a non-empty folder: %v", err)
	}
}

func TestSealRefusalsShowHomePaths(t *testing.T) {
	e := homeEnv(t)
	healthyRepo(t, e)
	inside := filepath.Join(e.root, "src")
	os.MkdirAll(inside, 0o755)
	err := e.app.Seal(inside, e.root, false)
	if err == nil || !strings.Contains(err.Error(), "source ~/backup/src and repository ~/backup must not contain each other") {
		t.Fatalf("seal from inside the repo: %v", err)
	}
	file := filepath.Join(e.app.Home, "notes.md")
	os.WriteFile(file, []byte("x"), 0o644)
	err = e.app.Seal(file, e.root, false)
	if err == nil || !strings.Contains(err.Error(), "~/notes.md is not a directory") {
		t.Fatalf("seal from a file: %v", err)
	}
}
