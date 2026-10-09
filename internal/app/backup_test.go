package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/gitx"
	"github.com/spicy-lemonade/salt/internal/guard"
	"github.com/spicy-lemonade/salt/internal/preset"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
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

// A backup seals only what the presets find, then stages, commits, checks
// origin, prunes and pushes, and prints nothing. Origin is checked before
// prune rewrites history, and the push is leased to what it found there.
func TestBackup(t *testing.T) {
	e, _, presets := backupEnv(t)
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); out != "" {
		t.Fatalf("backup printed %q", out)
	}
	if want := []string{"stage", "commit salt backup", "lease", "prune", "push"}; !slices.Equal(e.git.calls, want) {
		t.Fatalf("git calls = %v, want %v", e.git.calls, want)
	}
	if !slices.Equal(e.git.leased, []string{"tip"}) {
		t.Fatalf("pushes were leased to %v", e.git.leased)
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

// A file left out because it could not be checked says so, without the
// advice about secrets, since none was found.
func TestBackupLeavesOutUncheckedFile(t *testing.T) {
	e, home, presets := backupEnv(t)
	os.WriteFile(filepath.Join(home, "tool", "settings.yaml"), []byte("api_key: [unclosed"), 0o644)
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	want := "salt: left ~/tool/settings.yaml out of the backup because it could not be read as YAML or JSON to check it for secrets\n"
	if out := e.ui.out.String(); out != want {
		t.Fatalf("output %q, want %q", out, want)
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
		"detached": {func(e *testEnv, _ string) { e.git.branchErr = gitx.ErrDetached },
			"no branch is checked out"},
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
		"lease": {func(g *fakeGit) { g.lease = func(context.Context) (string, error) { return "", gitx.ErrRemoteMoved } },
			"salt could not check origin, so old backups were not dropped and it was not pushed: origin's branch", []string{"stage", "commit salt backup", "lease"}, false},
		"prune": {func(g *fakeGit) { g.prune, g.pruneErr = nil, boom },
			"the backup was committed, but dropping old backups failed, so it was not pushed: boom", []string{"stage", "commit salt backup", "lease", "prune"}, true},
		"head": {func(g *fakeGit) { g.headErr = boom },
			"the backup was committed but not pushed: boom", []string{"stage", "commit salt backup", "lease", "prune"}, true},
		"push": {func(g *fakeGit) { g.push = func(context.Context) error { return boom } },
			"the backup was committed but not pushed: boom", []string{"stage", "commit salt backup", "lease", "prune", "push"}, true},
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
	// A context cancelled before the backup stops it before anything is
	// sealed or git is asked anything.
	e.git.calls = nil
	e.git.storageCalls = 0
	if err := e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 1, Context: ctx}); !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "nothing was backed up") || len(e.git.calls) != 0 {
		t.Fatalf("backup = %v, calls %v", err, e.git.calls)
	}
}

// A signal during a git step stops the backup before the next one: above
// all, prune never rewrites history after Ctrl-C.
func TestBackupStopsBetweenSteps(t *testing.T) {
	for name, tc := range map[string]struct {
		during func(g *fakeGit, cancel func())
		want   string
		calls  []string
	}{
		"stage": {func(g *fakeGit, cancel func()) { g.onStage = cancel },
			"sealed and staged but not committed", []string{"stage"}},
		"commit": {func(g *fakeGit, cancel func()) {
			g.onCommit = func() error { cancel(); return context.Canceled }
		}, "sealed and staged but not committed", []string{"stage", "commit salt backup"}},
		"commit done": {func(g *fakeGit, cancel func()) { g.onCommit = func() error { cancel(); return nil } },
			"old backups were not dropped and it was not pushed", []string{"stage", "commit salt backup"}},
		"lease": {func(g *fakeGit, cancel func()) {
			g.lease = func(context.Context) (string, error) { cancel(); return "", context.Canceled }
		}, "old backups were not dropped and it was not pushed", []string{"stage", "commit salt backup", "lease"}},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, presets := backupEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.during(e.git, cancel)
			err := e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 1, Context: ctx})
			if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backup = %v", err)
			}
			if !slices.Equal(e.git.calls, tc.calls) || len(e.git.pruneDays) != 0 {
				t.Fatalf("git calls %v, pruned %v", e.git.calls, e.git.pruneDays)
			}
		})
	}
}

// Each push is told the commits this machine pushed, or tried to push, to
// origin since its last successful push, so a push whose answer was lost
// still counts as this machine's on the next run.
func TestBackupRemembersPushes(t *testing.T) {
	e, _, presets := backupEnv(t)
	boom := errors.New("connection dropped")
	steps := []struct {
		head   string
		failed error
	}{{"h1", nil}, {"h2", boom}, {"h3", boom}, {"h4", nil}, {"h5", nil}}
	for _, st := range steps {
		e.git.head = st.head
		e.git.push = func(context.Context) error { return st.failed }
		if err := e.backup(presets); !errors.Is(err, st.failed) {
			t.Fatalf("%s: backup = %v", st.head, err)
		}
	}
	want := [][]string{nil, {"h1"}, {"h1", "h2"}, {"h1", "h2", "h3"}, {"h4"}}
	if len(e.git.known) != len(want) {
		t.Fatalf("pushes were told %v", e.git.known)
	}
	for i := range want {
		if !slices.Equal(e.git.known[i], want[i]) {
			t.Errorf("push %d was told %v, want %v", i+1, e.git.known[i], want[i])
		}
	}
}

// A damaged record of pushes is set aside, and a record that cannot be
// written stops the push before it starts.
func TestBackupPushRecordProblems(t *testing.T) {
	e, _, presets := backupEnv(t)
	p := filepath.Join(e.root, ".git", "salt", "pushed.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("{not json"), 0o600)
	if err := e.backup(presets); err != nil || e.git.known[0] != nil {
		t.Fatalf("backup = %v, push told %v", err, e.git.known)
	}
	os.Remove(p)
	os.Mkdir(p, 0o700) // a folder where the record goes cannot be replaced
	os.WriteFile(filepath.Join(p, "x"), nil, 0o600)
	e.git.calls = nil
	if err := e.backup(presets); err == nil || slices.Contains(e.git.calls, "push") {
		t.Fatalf("backup = %v, calls %v", err, e.git.calls)
	}
}

// The record holds at most maxKnownPushes commits, and always the one the
// last push that worked left on origin.
func TestBackupCapsThePushRecord(t *testing.T) {
	e, _, presets := backupEnv(t)
	e.git.head = "pushed"
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	e.git.push = func(context.Context) error { return errors.New("offline") }
	for i := range maxKnownPushes + 5 {
		e.git.head = fmt.Sprint("h", i)
		e.backup(presets)
	}
	last := e.git.known[len(e.git.known)-1]
	if len(last) != maxKnownPushes || last[0] != "pushed" || last[len(last)-1] != fmt.Sprint("h", maxKnownPushes+3) {
		t.Fatalf("last push was told %d commits: %v … %v", len(last), last[0], last[len(last)-1])
	}
}

// While another salt works on the repo, backup, seal and prune each refuse
// at once, before changing anything, and work again once it is done.
//
// The lock is in the repo's .git folder, so a salt with another cache
// folder, as cron can have when XDG_CACHE_HOME is set only in the person's
// shell, is refused too.
func TestRepoLock(t *testing.T) {
	e, _, presets := backupEnv(t)
	unlock, err := guard.Lock(filepath.Join(e.root, ".git", "salt", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	e.app.CacheDir = t.TempDir()
	for name, run := range map[string]func() error{
		"backup": func() error { return e.backup(presets) },
		"seal":   func() error { return e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}) },
		"prune":  func() error { return e.app.Prune(e.root, 3) },
	} {
		if err := run(); !errors.Is(err, guard.ErrLocked) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(e.git.calls) != 0 || len(e.git.pruneDays) != 0 {
		t.Fatalf("git was asked %v, pruned %v", e.git.calls, e.git.pruneDays)
	}
	unlock()
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	// A repo that has gone cannot be locked.
	if _, err := e.app.lockRepo(filepath.Join(t.TempDir(), "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lockRepo on a missing repo: %v", err)
	}
}

// A repo with no .git folder, which salt seal accepts, is locked through a
// file in the cache folder instead.
func TestRepoLockWithoutGit(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := os.RemoveAll(filepath.Join(e.root, ".git")); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(e.root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := seal.RepoFile(e.app.CacheDir, real, "lock-", "")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := guard.Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); !errors.Is(err, guard.ErrLocked) {
		t.Fatalf("seal: %v", err)
	}
	unlock()
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err != nil {
		t.Fatal(err)
	}
}

// A linked worktree is locked in the git folder it shares with the main
// one, so salt in two worktrees of a repo never works on it at once.
func TestRepoLockInALinkedWorktree(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	main := filepath.Join(t.TempDir(), "main", ".git")
	own := filepath.Join(main, "worktrees", "backup")
	os.MkdirAll(own, 0o700)
	os.WriteFile(filepath.Join(own, "commondir"), []byte("../..\n"), 0o600)
	if err := os.RemoveAll(filepath.Join(e.root, ".git")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, ".git"), []byte("gitdir: "+own+"\n"), 0o600)
	unlock, err := guard.Lock(filepath.Join(main, "salt", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); !errors.Is(err, guard.ErrLocked) {
		t.Fatalf("seal: %v", err)
	}
	unlock()
}

func TestSharedGitDir(t *testing.T) {
	base := t.TempDir()
	dir := func(parts ...string) string {
		p := filepath.Join(append([]string{base}, parts...)...)
		os.MkdirAll(p, 0o700)
		return p
	}
	file := func(p, text string) { os.WriteFile(p, []byte(text), 0o600) }

	plain := dir("plain")
	withGit := dir("repo")
	dir("repo", ".git")
	// A submodule's .git names its own folder, relative, with no commondir.
	sub := dir("sub")
	modules := dir("repo", ".git", "modules", "sub")
	file(filepath.Join(sub, ".git"), "gitdir: ../repo/.git/modules/sub\n")
	linked := dir("linked")
	own := dir("repo", ".git", "worktrees", "linked")
	file(filepath.Join(linked, ".git"), "gitdir: "+own)
	file(filepath.Join(own, "commondir"), "../..")
	broken := dir("broken")
	file(filepath.Join(broken, ".git"), "not a git file")
	empty := dir("empty")
	file(filepath.Join(empty, ".git"), "gitdir: ")
	for root, want := range map[string]string{
		plain:   "",
		withGit: filepath.Join(withGit, ".git"),
		sub:     modules,
		linked:  filepath.Join(withGit, ".git"),
		broken:  "",
		empty:   "",
	} {
		if got := sharedGitDir(root); got != want {
			t.Errorf("sharedGitDir(%s) = %q, want %q", root, got, want)
		}
	}
}

// The record of pushes is kept in the repo's .git folder, so a backup run
// with another cache folder still knows what this machine pushed.
func TestBackupPushRecordOutlivesTheCache(t *testing.T) {
	e, _, presets := backupEnv(t)
	e.git.push = func(context.Context) error { return errors.New("connection dropped") }
	e.backup(presets)
	e.app.CacheDir = t.TempDir()
	e.git.push = nil
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if got := e.git.known[1]; !slices.Equal(got, []string{"h1"}) {
		t.Fatalf("the second lease was told %v, want [h1]", got)
	}
	if _, err := os.Stat(filepath.Join(e.root, ".git", "salt", "pushed.json")); err != nil {
		t.Fatal(err)
	}
}

// A setting named like a secret that holds only a number is not taken for
// one, so its file is backed up, with a warning naming the setting but not
// its value.
func TestBackupWarnsOfNumberSecrets(t *testing.T) {
	e, home, presets := backupEnv(t)
	os.WriteFile(filepath.Join(home, "tool", "settings.yaml"), []byte("api_key: 98765"), 0o644)
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	want := "salt: warning: the setting api_key in ~/tool/settings.yaml holds a number, which salt does not take for a secret, so the file is backed up. If it is a secret, keep it in an environment variable\n"
	if out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.restored(); got["tool/settings.yaml"] != "api_key: 98765" {
		t.Fatalf("restored %v", got)
	}
}

// A place backed up last time and not found now, such as a profile whose
// folder is gone, is named once, and the backup goes on. The next backup
// no longer holds it, so it is not named again.
func TestBackupWarnsOfMissingPlaces(t *testing.T) {
	e, home, presets := backupEnv(t)
	write := func(rel string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(rel), 0o644)
	}
	write("tool/profiles/home/notes.md")
	write("tool/profiles/home/sub/more.md")
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); out != "" {
		t.Fatalf("the first backup printed %q", out)
	}
	os.RemoveAll(filepath.Join(home, "tool", "profiles", "home"))
	os.Remove(filepath.Join(home, "tool", "profiles", "work", "notes.md")) // a file gone from a place still found
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	want := "salt: tool/profiles/home was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n"
	if out := e.ui.out.String(); out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.restored(); !mapsEqual(got, map[string]string{"tool/notes.md": "notes", "tool/settings.yaml": "level: 3"}) {
		t.Fatalf("restored %v", got)
	}
	e.ui.out.Reset()
	if err := e.backup(presets); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); out != "" {
		t.Fatalf("the third backup printed %q", out)
	}
}

// A single-file place deleted after it was found but before it was sealed
// is named as missing when the last backup held it, and the backup goes on.
// When that leaves a preset with nothing, the backup stops before it is
// staged or committed.
func TestBackupPlaceGoneBeforeSealed(t *testing.T) {
	for name, only := range map[string]bool{"named": false, "preset left with nothing": true} {
		t.Run(name, func(t *testing.T) {
			e, home, presets := backupEnv(t)
			if only {
				p, err := preset.Parse("only", []byte(`{"name": "only", "paths": [{"from": "${TOOL_HOME}/notes.md", "to": "tool/notes.md"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				presets = append(presets, p)
			}
			if err := e.backup(presets); err != nil {
				t.Fatal(err)
			}
			// A symlink is named after the presets gather and before seal
			// reads anything, so the file is deleted then. This keeps the hook
			// in the fake UI rather than in salt's own code; if that line
			// ever moves after sealing, the file is not deleted in time and
			// the test fails rather than passing wrongly.
			os.Symlink("notes.md", filepath.Join(home, "tool", "profiles", "work", "link.md"))
			e.ui.onPrintf = func(line string) {
				if strings.HasPrefix(line, "salt: skipped") {
					os.Remove(filepath.Join(home, "tool", "notes.md"))
				}
			}
			e.ui.out.Reset()
			e.git.calls = nil
			err := e.backup(presets)
			if only {
				if !errors.Is(err, preset.ErrNothing) || !strings.Contains(err.Error(), "the backup was sealed but not committed") || !strings.Contains(err.Error(), "for the only preset") {
					t.Fatalf("Backup = %v", err)
				}
				if len(e.git.calls) != 0 {
					t.Fatalf("calls = %v", e.git.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			const want = "salt: tool/notes.md was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n"
			if out := e.ui.out.String(); !strings.HasSuffix(out, want) {
				t.Fatalf("output %q, want it to end %q", out, want)
			}
			if _, ok := e.restored()["tool/notes.md"]; ok {
				t.Fatal("the deleted file was restored")
			}
		})
	}
}

// warnMissing names a place inside another that is found, one around
// another that is found, and only the outer one when both are missing. A place still found, or with something
// in this backup, is not named, nor is a path no preset names.
func TestWarnMissingNestedPlaces(t *testing.T) {
	p, err := preset.Parse("t", []byte(`{"name": "t", "paths": [
		{"from": "~/outer", "to": "outer"},
		{"from": "~/inner", "to": "outer/inner"},
		{"from": "~/one.md", "to": "one.md"},
		{"from": "~/empty", "to": "empty"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	last := []string{"outer/a.md", "outer/inner/b.md", "one.md", "empty/c.md", "elsewhere.md"}
	for _, tc := range []struct {
		name    string
		found   preset.Found
		gone    []string
		missing []string
	}{
		{"inner missing", preset.Found{Places: []string{"outer", "one.md", "empty"}, Files: []seal.Extra{{Rel: "outer/a.md"}, {Rel: "one.md"}}}, nil, []string{"outer/inner"}},
		{"outer missing", preset.Found{Places: []string{"outer/inner", "one.md", "empty"}, Files: []seal.Extra{{Rel: "outer/inner/b.md"}, {Rel: "one.md"}}}, nil, []string{"outer"}},
		{"both missing", preset.Found{Places: []string{"one.md", "empty"}, Files: []seal.Extra{{Rel: "one.md"}}}, nil, []string{"outer"}},
		{"inner held by outer", preset.Found{Places: []string{"outer", "one.md", "empty"}, Files: []seal.Extra{{Rel: "outer/inner/b.md"}, {Rel: "one.md"}}}, nil, nil},
		{"file gone before sealed", preset.Found{Places: []string{"outer", "outer/inner", "one.md", "empty"}, Files: []seal.Extra{{Rel: "outer/a.md"}, {Rel: "one.md"}}}, []string{"one.md"}, []string{"one.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ui.out.Reset()
			tc.found.Drop(tc.gone)
			e.app.warnMissing(last, []*preset.Preset{p}, &tc.found)
			var want strings.Builder
			for _, m := range tc.missing {
				want.WriteString("salt: " + m + " was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n")
			}
			if out := e.ui.out.String(); out != want.String() {
				t.Fatalf("output %q, want %q", out, want.String())
			}
		})
	}
}

// warnMissing names a database a preset names, which the last backup held
// and this one does not, as when its connection's variable is not set for
// cron, and not when it is backed up. One inside a missing place is not
// named again.
func TestWarnMissingDatabase(t *testing.T) {
	p, err := preset.Parse("t", []byte(`{"name": "t", "paths": [{"from": "~/tool", "to": "tool"}],
		"databases": [{"kind": "postgres", "from": "${T_DB}", "to": "tool/memory.sql"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	db, err := source.NewPostgres("postgresql://localhost/memory")
	if err == nil {
		db, err = source.Named(db, "tool/memory.sql")
	}
	if err != nil {
		t.Fatal(err)
	}
	last := []string{"tool/a.md", "tool/memory.sql"}
	for _, tc := range []struct {
		name    string
		found   preset.Found
		missing string
	}{
		{"database missing", preset.Found{Places: []string{"tool"}, Files: []seal.Extra{{Rel: "tool/a.md"}}}, "tool/memory.sql"},
		{"database found", preset.Found{Places: []string{"tool"}, Files: []seal.Extra{{Rel: "tool/a.md"}}, Databases: []source.Database{db}}, ""},
		{"both missing", preset.Found{}, "tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ui.out.Reset()
			e.app.warnMissing(last, []*preset.Preset{p}, &tc.found)
			want := ""
			if tc.missing != "" {
				want = "salt: " + tc.missing + " was in the last backup but was not found this time, so it is no longer backed up. Its earlier copies stay in history until prune drops them\n"
			}
			if out := e.ui.out.String(); out != want {
				t.Fatalf("output %q, want %q", out, want)
			}
		})
	}
}

// Before pushing, a backup measures the push against what origin holds and
// warns when it is large, without stopping it. It says nothing when the
// push is small or cannot be measured, as with an older git.
func TestBackupWarnsOfALargePush(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		err  error
		warn bool
	}{
		{"small", warnPushBytes, nil, false},
		{"large", 3 << 30, nil, true},
		{"cannot measure", 3 << 30, errors.New("unknown option"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, presets := backupEnv(t)
			e.git.pushSize, e.git.pushSizeErr = tc.size, tc.err
			e.ui.out.Reset()
			if err := e.backup(presets); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(e.git.sizeLeases, []string{"tip"}) || !slices.Contains(e.git.calls, "push") {
				t.Fatalf("measured against %v, calls %v", e.git.sizeLeases, e.git.calls)
			}
			const want = "salt: warning: this push sends up to about 3.0 GiB. GitHub refuses a push over 2 GB"
			if out := e.ui.out.String(); strings.Contains(out, want) != tc.warn {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

// A file that added much new ciphertext is named, never shown.
func TestWarnPushNamesLargeFiles(t *testing.T) {
	e := newEnv(t)
	e.ui.out.Reset()
	e.app.warnPush(context.Background(), e.root, "", []seal.Written{
		{Path: "tool/state.db", Bytes: warnFileBytes + 1},
		{Path: "tool/notes.md", Bytes: warnFileBytes},
		{Path: "tool/huge.db", Bytes: 5 << 30},
	})
	out := e.ui.out.String()
	for _, want := range []string{
		"salt: warning: tool/state.db added 500 MiB of encrypted data to this backup. A large file that changes often makes the backup repo grow quickly, so back it up once a day at most",
		"salt: warning: tool/huge.db added 5.0 GiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notes.md") {
		t.Errorf("a file at the limit was named:\n%s", out)
	}
}

// Ctrl-C while the push is measured stops the backup before prune, whether
// salt sees the signal first or git, which then dies before ctx is
// cancelled.
func TestBackupInterruptedWhileMeasuringThePush(t *testing.T) {
	for name, gitFirst := range map[string]bool{"salt first": false, "git first": true} {
		t.Run(name, func(t *testing.T) {
			e, _, presets := backupEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if gitFirst {
				e.git.pushSizeErr = context.Canceled
			} else {
				e.git.onPushSize = cancel
			}
			err := e.app.Backup(BackupOptions{Repo: e.root, Presets: presets, KeepDays: 3, Context: ctx})
			if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "old backups were not dropped and it was not pushed") {
				t.Fatalf("Backup = %v", err)
			}
			if slices.Contains(e.git.calls, "prune") || slices.Contains(e.git.calls, "push") {
				t.Fatalf("calls = %v", e.git.calls)
			}
		})
	}
}
