package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/app"
	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/preset"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/source"
	"github.com/zalando/go-keyring"
)

// Two locks on the real keychain: salt's own file store, and go-keyring's
// in-memory mock in case anything still reaches the keychain code.
//
// git and database programs are replaced too: unit tests must never start
// them.
func TestMain(m *testing.M) {
	os.Setenv("SALT_KEYSTORE", "file")
	keyring.MockInit()
	gitOps = testGit
	databaseKinds = testKinds()
	os.Exit(m.Run())
}

// testKinds keeps every real kind's option and parsing, but copies each
// database as a plain file, so no database program is started.
func testKinds() []source.Kind {
	kinds := slices.Clone(source.Kinds)
	for i := range kinds {
		real := kinds[i].New
		kinds[i].New = func(arg string) (source.Database, error) {
			if _, err := real(arg); err != nil {
				return nil, err
			}
			return fileDB(arg), nil
		}
	}
	return kinds
}

// fileDB stands in for a live database with a plain copy of the file.
type fileDB string

func (f fileDB) Name() string   { return filepath.Base(string(f)) }
func (f fileDB) String() string { return string(f) }
func (f fileDB) Flag() string   { return "--test" }
func (f fileDB) Copy(_ context.Context, o source.CopyOptions) (source.Meta, error) {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return source.Meta{}, err
	}
	return source.Meta{Mode: 0o600}, os.WriteFile(o.Dst, b, 0o600)
}

// noGit answers salt's questions for git without starting it.
type noGit struct {
	storageCalls int
	pruneDays    []int
}

var testGit = &noGit{}

func (*noGit) HookPath(string) (string, error)             { return "", errors.New("no git in unit tests") }
func (*noGit) Staged(string) ([]check.Violation, error)    { return nil, nil }
func (*noGit) Committed(string) ([]check.Violation, error) { return nil, nil }
func (*noGit) LastCommit(string) (time.Time, bool, error)  { return time.Time{}, false, nil }
func (*noGit) Remote(string) string                        { return "" }
func (g *noGit) Storage(string) ([]check.StorageProblem, int, error) {
	g.storageCalls++
	return nil, 0, nil
}

func (g *noGit) Clone(context.Context, string, string) error {
	return errors.New("no git in unit tests")
}

func (*noGit) Branch(string) (string, error)                { return "main", nil }
func (*noGit) Stage(string) error                           { return nil }
func (*noGit) Commit(context.Context, string, string) error { return nil }
func (*noGit) Head(string) (string, error)                  { return "h", nil }
func (*noGit) Push(context.Context, string, []string) error { return nil }

func (g *noGit) Prune(_ string, keepDays int) (*prune.Result, error) {
	g.pruneDays = append(g.pruneDays, keepDays)
	return &prune.Result{Kept: 1, Days: 1}, nil
}

// isolate points HOME and the config and cache dirs at a temp dir.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
}

func TestParse(t *testing.T) {
	fs := newFlags("x")
	force := fs.Bool("force", false, "")
	to := fs.String("to", "", "")
	pos, err := parse(fs, []string{"a", "--force", "b", "--to", "dir", "c"}, 1, -1)
	if err != nil || strings.Join(pos, ",") != "a,b,c" || !*force || *to != "dir" {
		t.Fatalf("parse = %v, %v, force=%v to=%q", pos, err, *force, *to)
	}
	var ue usageError
	if _, err := parse(newFlags("x"), []string{"a", "b"}, 1, 1); !errors.As(err, &ue) {
		t.Fatalf("too many args: %v", err)
	}
	if _, err := parse(newFlags("x"), nil, 1, 1); !errors.As(err, &ue) {
		t.Fatalf("too few args: %v", err)
	}
	if _, err := parse(newFlags("x"), []string{"--nope"}, 0, 1); !errors.As(err, &ue) || ue.Error() == "" {
		t.Fatalf("unknown flag: %v", err)
	}
}

func TestOrDotAndStoreName(t *testing.T) {
	if orDot(nil) != "." || orDot([]string{"r"}) != "r" {
		t.Fatal("orDot")
	}
	if storeName() == "" {
		t.Fatal("storeName empty")
	}
}

func TestRunUsageErrors(t *testing.T) {
	isolate(t)
	aPreset := preset.Names()[0]
	for _, args := range [][]string{
		{"nope"},
		{"init"},
		{"seal", "only-one"},
		{"check", "a", "b"},
		{"restore", "repo"},
		{"recovery"},
		{"recovery", "bogus", "repo"},
		{"recovery", "test"},
		{"verify"},
		{"doctor", "a", "b"},
		{"hook"},
		{"hook", "install", "a", "b"},
		{"trust"},
		{"trust", "a", "b"},
		{"prune"},
		{"prune", "a", "b"},
		{"prune", "--keep-days", "0", "a"},
		{"prune", "--keep-days", "-3", "a"},
		{"prune", "--keep-days", "five", "a"},
		{"backup", "repo"},
		{"backup", "--preset", "nope", "repo"},
		{"backup", "--preset", aPreset, "--preset", aPreset, "repo"},
		{"backup", "--preset", aPreset, "--keep-days", "0", "repo"},
		{"backup", "--preset", aPreset},
		{"backup", "--preset", aPreset, "a", "b"},
	} {
		var ue usageError
		if err := run(args[0], args[1:]); !errors.As(err, &ue) {
			t.Errorf("run %v: err = %v, want usage error", args, err)
		}
	}
	for _, c := range []string{"version", "--version", "help", "-h"} {
		if err := run(c, nil); err != nil {
			t.Errorf("run %s: %v", c, err)
		}
	}
}

// salt backup lists the presets, and reaches the backup itself once its
// options are right.
func TestRunBackup(t *testing.T) {
	isolate(t)
	names := strings.Join(preset.Names(), ", ")
	if !strings.Contains(usage, "Presets: "+names+".") || strings.Contains(usage, "{presets}") {
		t.Fatal("usage does not list the presets")
	}
	err := run("backup", []string{"--preset", "nope", "repo"})
	if !strings.Contains(err.Error(), `unknown preset "nope"; the presets are `+names) {
		t.Fatalf("unknown preset: %v", err)
	}
	var ue usageError
	err = run("backup", []string{"--preset", preset.Names()[0], "--keep-days", "2", t.TempDir()})
	if err == nil || errors.As(err, &ue) || !strings.Contains(err.Error(), "not a salt repository") {
		t.Fatalf("backup: %v", err)
	}
}

// A --name that names no database option, or that is unsafe, is a usage
// error, refused before any database is copied, that never repeats a
// password.
func TestRunSealNameUsage(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	db := filepath.Join(base, "state.db")
	os.WriteFile(db, []byte("state"), 0o644)
	for want, args := range map[string][]string{
		"seal: --name must be followed by the database option it names": {"--name", "a.db", "--sqlite", db, "--name", "spare.db"},
		"seal: invalid value \"b.db\" for flag -name: --name must be":   {"--name", "a.db", "--name", "b.db", "--sqlite", db},
		"seal: the name \"../state.db\" cannot be used in the backup":   {"--name", "../state.db", "--postgres", "postgresql://agent:hunter2@localhost:5432/postgres"},
		"seal: the name \"agent2/\" cannot be used in the backup":       {"--name", "agent2/", "--sqlite", db},
	} {
		var ue usageError
		if err := run("seal", append(args, base, filepath.Join(base, "repo"))); !errors.As(err, &ue) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("seal %v: %v", args, err)
		}
	}
}

// seal, verify and restore end to end in-process. None of them runs git.
func TestRunSealVerifyRestore(t *testing.T) {
	isolate(t)
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	rcpt := id.Recipient().String()
	if err := (keys.FileStore{Dir: filepath.Join(cfg, "salt", "keys")}).Set(rcpt, keys.IdentitySecret(id)); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	root, src := filepath.Join(base, "repo"), filepath.Join(base, "src")
	if err := repo.Write(root, repo.Format{Version: repo.FormatVersion, EncryptPaths: true, Recovery: repo.RecoveryPassphrase}, []string{rcpt}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "memories"), 0o755)
	os.WriteFile(filepath.Join(src, "memories", "USER.md"), []byte("hello"), 0o644)
	// A git repo, so seal goes on to ask git (the fake) about storage.
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)

	// A repo this machine hasn't approved is refused until `salt trust`.
	if err := run("seal", []string{"--prune", src, root}); !errors.Is(err, app.ErrNotTrusted) {
		t.Fatalf("seal before trust: %v", err)
	}
	if err := run("trust", []string{root}); !errors.Is(err, app.ErrNotInteractive) {
		t.Fatalf("trust without a terminal or --yes: %v", err)
	}
	if err := run("trust", []string{"--yes", root}); err != nil {
		t.Fatalf("trust --yes: %v", err)
	}
	// Each --sqlite database is copied and sealed at the top of the backup.
	dbs := filepath.Join(base, "agent")
	os.MkdirAll(dbs, 0o755)
	os.WriteFile(filepath.Join(dbs, "state.db"), []byte("state"), 0o644)
	os.WriteFile(filepath.Join(dbs, "memory.db"), []byte("memory"), 0o644)
	calls := testGit.storageCalls
	if err := run("seal", []string{"--prune", "--sqlite", filepath.Join(dbs, "state.db"), src, root, "--sqlite", filepath.Join(dbs, "memory.db")}); err != nil {
		t.Fatalf("seal: %v", err)
	}
	// A database the person names must exist; only a preset's may vanish.
	missing := filepath.Join(dbs, "missing.db")
	if err := run("seal", []string{"--sqlite", missing, src, root}); err == nil || !strings.Contains(err.Error(), "missing.db does not exist; check the path given to") {
		t.Fatalf("seal --sqlite missing.db: %v", err)
	}
	if testGit.storageCalls != calls+1 {
		t.Fatalf("seal asked git about storage %d time(s), want once", testGit.storageCalls-calls)
	}
	if err := run("verify", []string{root}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	dest := filepath.Join(base, "restored")
	if err := run("restore", []string{root, "--to", dest}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for rel, want := range map[string]string{"memories/USER.md": "hello", "state.db": "state", "memory.db": "memory"} {
		if b, _ := os.ReadFile(filepath.Join(dest, rel)); string(b) != want {
			t.Fatalf("restored %s = %q, want %q", rel, b, want)
		}
	}
	// --name backs the database option after it up under the chosen name, so
	// two databases called state.db are both backed up and restored.
	other := filepath.Join(base, "agent2", "state.db")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("other state"), 0o644)
	if err := run("seal", []string{"--sqlite", filepath.Join(dbs, "state.db"), "--name", "agent2/state.db", "--sqlite", other, src, root}); err != nil {
		t.Fatalf("seal --name: %v", err)
	}
	named := filepath.Join(base, "named")
	if err := run("restore", []string{root, "--to", named}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for rel, want := range map[string]string{"state.db": "state", "agent2/state.db": "other state"} {
		if b, _ := os.ReadFile(filepath.Join(named, rel)); string(b) != want {
			t.Fatalf("restored %s = %q, want %q", rel, b, want)
		}
	}
	// trust --yes set up this machine's signing key next to the approvals.
	if _, err := (keys.FileStore{Dir: filepath.Join(cfg, "salt", "signing")}).Get(rcpt); err != nil {
		t.Fatalf("no signing key saved: %v", err)
	}
	// --allow-unsigned is accepted by verify and restore; a signed backup
	// needs no warning.
	if err := run("verify", []string{"--allow-unsigned", root}); err != nil {
		t.Fatalf("verify --allow-unsigned: %v", err)
	}
	if err := run("restore", []string{root, "--allow-unsigned", "--to", filepath.Join(base, "again")}); err != nil {
		t.Fatalf("restore --allow-unsigned: %v", err)
	}
	// A database option's value is checked before anything is sealed, and a
	// password in it is never repeated.
	t.Setenv("SALT_TEST_EMPTY", "")
	t.Setenv("PGDATABASE", "")
	for _, args := range [][]string{
		{"--postgres", "postgresql://agent:hunter2@localhost:5432"},
		{"--postgres", "mysql://agent:hunter2@localhost/memory"},
		{"--postgres-env", "SALT_TEST_EMPTY"},
	} {
		err := run("seal", append(args, src, root))
		var ue usageError
		if err == nil || errors.As(err, &ue) || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("seal %v: %v", args, err)
		}
	}
	// Recovery commands need a terminal; tests have none.
	if err := run("recovery", []string{"test", root}); !errors.Is(err, app.ErrNotInteractive) {
		t.Fatalf("recovery test: %v", err)
	}
	if err := run("recovery", []string{"show", root}); err == nil {
		t.Fatal("recovery show on a passphrase repo succeeded")
	}
	// prune passes the number of days on, 5 unless --keep-days says otherwise.
	testGit.pruneDays = nil
	if err := run("prune", []string{root}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := run("prune", []string{"--keep-days", "1", root}); err != nil {
		t.Fatalf("prune --keep-days 1: %v", err)
	}
	if !slices.Equal(testGit.pruneDays, []int{5, 1}) {
		t.Fatalf("prune asked git for %v days, want [5 1]", testGit.pruneDays)
	}
	if err := run("prune", []string{filepath.Join(base, "not-a-repo")}); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("prune of a non-salt dir: %v", err)
	}
	if err := run("seal", []string{src, filepath.Join(base, "not-a-repo")}); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("seal into a non-salt dir: %v", err)
	}
}

// The keychain-fallback warning and the file store's name show the key folder
// inside home as ~/….
func TestNewAppShowsKeyDirAsHomePath(t *testing.T) {
	isolate(t)
	cfg, _ := os.UserConfigDir() // ~/.config on Linux, ~/Library/Application Support on macOS
	want := app.ShortPath(filepath.Join(cfg, "salt", "keys"), os.Getenv("HOME"))
	if !strings.HasPrefix(want, "~") {
		t.Fatalf("config dir %q is not inside the test home", cfg)
	}
	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	if a.StoreName != "private key file under "+want {
		t.Errorf("StoreName = %q", a.StoreName)
	}

	t.Setenv("SALT_KEYSTORE", "")
	errFile, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = errFile // newApp's terminal writes here
	t.Cleanup(func() { os.Stderr = stderr })
	if a, err = newApp(); err != nil {
		t.Fatal(err)
	}
	fs, ok := a.Store.(keys.FallbackStore)
	if !ok {
		t.Fatalf("store is %T, want keys.FallbackStore", a.Store)
	}
	fs.Warn(errors.New("locked"))
	out, _ := os.ReadFile(errFile.Name())
	if !strings.Contains(string(out), "saving the key to a private file under "+want+" instead") {
		t.Errorf("warning = %q", out)
	}
}

// Without a home folder or config folder, newApp refuses instead of guessing.
// On Linux this fails on the config folder; macOS fails on the cache folder.
func TestNewAppWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if _, err := newApp(); err == nil {
		t.Fatal("newApp without a home folder succeeded")
	}
}
