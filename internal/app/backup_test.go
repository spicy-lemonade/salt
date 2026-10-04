package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/preset"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/seal"
)

// backupEnv is a set-up backup repo and a home folder holding a tool's files,
// with a preset that backs them up. The files are plain, so no database
// program is started.
func backupEnv(t *testing.T) (e *testEnv, home string, presets []*preset.Preset) {
	t.Helper()
	e = newEnv(t)
	healthyRepo(t, e)
	home = t.TempDir()
	e.app.Home = home
	e.app.Getenv = func(k string) string { return map[string]string{"TOOL_HOME": filepath.Join(home, "tool")}[k] }
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tool/notes.md", "notes")
	write("tool/profiles/work/notes.md", "work notes")
	write("tool/settings.yaml", "level: 3")
	p, err := preset.Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "${TOOL_HOME}/notes.md", "to": "tool/notes.md"},
		{"from": "~/tool/profiles/*", "to": "tool/profiles/*"},
		{"from": "~/tool/settings.yaml", "to": "tool/settings.yaml"}],
		"secrets": [{"files": ["settings.yaml"], "keys": ["api_key"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.git.prune = &prune.Result{Kept: 1, Days: 1}
	return e, home, []*preset.Preset{p}
}

func (e *testEnv) backup(presets []*preset.Preset) error {
	return e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 3})
}

// restored restores the backup and returns its files and their contents.
func (e *testEnv) restored() map[string]string {
	e.t.Helper()
	dest := filepath.Join(e.t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		e.t.Fatal(err)
	}
	files := map[string]string{}
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dest, p)
			files[filepath.ToSlash(rel)] = string(b)
		}
		return err
	})
	return files
}

// A backup seals only what the presets find, then stages, commits, prunes
// and pushes, and prints nothing.
func TestBackup(t *testing.T) {
	e, _, presets := backupEnv(t)
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); out != "" {
		t.Fatalf("backup printed %q", out)
	}
	if want := []string{"stage", "commit salt backup", "push"}; !slices.Equal(e.git.calls, want) {
		t.Fatalf("git calls = %v, want %v", e.git.calls, want)
	}
	if !slices.Equal(e.git.pruneDays, []int{3}) {
		t.Fatalf("prune days = %v", e.git.pruneDays)
	}
	// The file healthyRepo sealed is gone: the repo holds only the presets'.
	want := map[string]string{"tool/notes.md": "notes", "tool/profiles/work/notes.md": "work notes", "tool/settings.yaml": "level: 3"}
	if got := e.restored(); !mapsEqual(got, want) {
		t.Fatalf("restored %v, want %v", got, want)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// A file holding a secret is left out with a warning, and so is anything
// that is not a file.
func TestBackupLeavesOutSecrets(t *testing.T) {
	e, home, presets := backupEnv(t)
	os.WriteFile(filepath.Join(home, "tool", "settings.yaml"), []byte("api_key: sk-123"), 0o644)
	os.Symlink("notes.md", filepath.Join(home, "tool", "profiles", "work", "link.md"))
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	for _, want := range []string{
		"salt: left ~/tool/settings.yaml out of the backup because its setting api_key holds a secret. Keep secrets in environment variables so the file can be backed up\n",
		"salt: skipped ~/tool/profiles/work/link.md (not a file)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "sk-123") {
		t.Fatal("the secret was shown")
	}
	if got := e.restored(); !mapsEqual(got, map[string]string{"tool/notes.md": "notes", "tool/profiles/work/notes.md": "work notes"}) {
		t.Fatalf("restored %v", got)
	}
}

// Nothing is sealed or sent to git when the backup cannot be pushed, is not
// a git repo, or the presets find nothing.
func TestBackupRefusesBeforeSealing(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(e *testEnv, home string)
		want   string
	}{
		"no remote": {func(e *testEnv, _ string) { e.git.remote = "" },
			"has no remote named origin to push the backup to. Add one with `git remote add origin URL`"},
		"not git": {func(e *testEnv, _ string) { os.RemoveAll(filepath.Join(e.root, ".git")) },
			"is not a git repository"},
		"nothing found": {func(_ *testEnv, home string) { os.RemoveAll(filepath.Join(home, "tool")) },
			"found nothing to back up for the t preset"},
		"repo inside": {func(e *testEnv, home string) {
			if err := os.Symlink(e.root, filepath.Join(home, "tool", "profiles", "repo")); err != nil {
				e.t.Fatal(err)
			}
		}, "must not contain each other"},
		"not a salt repo": {func(e *testEnv, _ string) { e.root = e.t.TempDir() },
			"not a salt repository"},
	} {
		t.Run(name, func(t *testing.T) {
			e, home, presets := backupEnv(t)
			tc.change(e, home)
			before := e.git.storageCalls
			err := e.backup(presets)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backup = %v, want %q", err, tc.want)
			}
			if len(e.git.calls) != 0 || e.git.storageCalls != before {
				t.Fatalf("git was asked %v", e.git.calls)
			}
		})
	}
}

// Two places the presets back up at the same path are refused before
// anything is sealed, blaming the presets, not the person.
func TestBackupRefusesClashingPaths(t *testing.T) {
	e, _, _ := backupEnv(t)
	p, err := preset.Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "~/tool/notes.md", "to": "same"},
		{"from": "~/tool/settings.yaml", "to": "same"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	err = e.backup([]*preset.Preset{p})
	if !errors.Is(err, seal.ErrDuplicatePath) || !strings.Contains(err.Error(), "the presets would back up same and same") || len(e.git.calls) != 0 {
		t.Fatalf("backup = %v, git calls %v", err, e.git.calls)
	}
}

// Each git step that fails stops the backup before the next one, and says
// what was done.
func TestBackupGitFailures(t *testing.T) {
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		change func(g *fakeGit)
		want   string
		calls  []string
		pruned bool
	}{
		"stage": {func(g *fakeGit) { g.stageErr = boom }, "staging the backup: boom", []string{"stage"}, false},
		"plaintext staged": {func(g *fakeGit) { g.staged = []check.Violation{{Path: "x.md", Reason: "not encrypted"}} },
			"problems reported", []string{"stage"}, false},
		"storage": {func(g *fakeGit) { g.storageTotal = 1 }, "problems reported", []string{"stage"}, false},
		"commit":  {func(g *fakeGit) { g.commitErr = boom }, "committing the backup: boom", []string{"stage", "commit salt backup"}, false},
		"prune": {func(g *fakeGit) { g.prune, g.pruneErr = nil, boom },
			"the backup was committed, but dropping old backups failed, so it was not pushed: boom", []string{"stage", "commit salt backup"}, true},
		"push": {func(g *fakeGit) { g.push = func(context.Context) error { return boom } },
			"the backup was committed but not pushed: boom", []string{"stage", "commit salt backup", "push"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, presets := backupEnv(t)
			tc.change(e.git)
			err := e.backup(presets)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backup = %v, want %q", err, tc.want)
			}
			if !slices.Equal(e.git.calls, tc.calls) || (len(e.git.pruneDays) > 0) != tc.pruned {
				t.Fatalf("git calls = %v, pruned %v", e.git.calls, e.git.pruneDays)
			}
		})
	}
}

// A prune that could not clean up after itself is a warning, and the backup
// is still pushed.
func TestBackupPruneCleanupWarns(t *testing.T) {
	e, _, presets := backupEnv(t)
	e.git.pruneErr = prune.ErrCleanup
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "salt: warning: ") || !slices.Contains(e.git.calls, "push") {
		t.Fatalf("output %q, calls %v", e.ui.out.String(), e.git.calls)
	}
}

// Ctrl-C during the push stops it and says the backup was not pushed.
func TestBackupInterruptedPush(t *testing.T) {
	e, _, presets := backupEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.git.push = func(context.Context) error {
		cancel()
		return context.Canceled
	}
	err := e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 1, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "committed but not pushed") {
		t.Fatalf("backup = %v", err)
	}
	// A context cancelled before the backup stops it after sealing, before
	// git is asked anything.
	e.git.calls = nil
	if err := e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 1, Context: ctx}); !errors.Is(err, ErrInterrupted) || len(e.git.calls) != 0 {
		t.Fatalf("backup = %v, calls %v", err, e.git.calls)
	}
}
