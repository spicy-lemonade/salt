package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/app"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/zalando/go-keyring"
)

// Two locks on the real keychain: salt's own file store, and go-keyring's
// in-memory mock in case anything still reaches the keychain code.
func TestMain(m *testing.M) {
	os.Setenv("SALT_KEYSTORE", "file")
	keyring.MockInit()
	os.Exit(m.Run())
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
	if err := run("seal", []string{"--prune", src, root}); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := run("verify", []string{root}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	dest := filepath.Join(base, "restored")
	if err := run("restore", []string{root, "--to", dest}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "memories", "USER.md")); string(b) != "hello" {
		t.Fatalf("restored %q", b)
	}
	// Recovery commands need a terminal; tests have none.
	if err := run("recovery", []string{"test", root}); !errors.Is(err, app.ErrNotInteractive) {
		t.Fatalf("recovery test: %v", err)
	}
	if err := run("recovery", []string{"show", root}); err == nil {
		t.Fatal("recovery show on a passphrase repo succeeded")
	}
	if err := run("seal", []string{src, filepath.Join(base, "not-a-repo")}); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("seal into a non-salt dir: %v", err)
	}
}
