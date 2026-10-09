package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spicy-lemonade/salt/internal/regular"
)

func TestInstall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks", "pre-commit")
	if err := Install(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("hook not executable: %v", err)
	}
	if !Installed(p) {
		t.Fatal("Installed() = false after Install")
	}
	// Re-installing over salt's own hook is fine.
	if err := Install(p); err != nil {
		t.Fatal(err)
	}
}

func TestInstallKeepsForeignHook(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pre-commit")
	os.WriteFile(p, []byte("#!/bin/sh\nnpm test\n"), 0o755)
	if err := Install(p); !errors.Is(err, ErrForeign) {
		t.Fatalf("Install over foreign hook: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "#!/bin/sh\nnpm test\n" {
		t.Fatal("foreign hook modified")
	}
}

func TestScriptCallsSaltByName(t *testing.T) {
	if !strings.Contains(Script, "exec salt check\n") {
		t.Fatal("hook must exec `salt check` by name")
	}
	if strings.Contains(Script, "/salt ") || strings.Contains(Script, ".test") {
		t.Fatal("hook must not reference a binary path")
	}
}

// A named pipe where the hook goes, which only a process on this machine can
// put there, is refused rather than waited on, by both install and doctor's
// check. Install never writes to it.
func TestHookRefusesAPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pre-commit")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := Install(p); !errors.Is(err, regular.ErrNotRegular) {
		t.Fatalf("Install over a pipe: %v", err)
	}
	if Installed(p) {
		t.Fatal("a pipe counts as salt's hook")
	}
}

// A hook over maxHook bytes is not salt's, even if it has salt's marker in
// it, so install keeps it and doctor's check does not count it.
func TestHookOverTheCapIsForeign(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pre-commit")
	big := Script + strings.Repeat("#\n", maxHook)
	os.WriteFile(p, []byte(big), 0o755)
	if err := Install(p); !errors.Is(err, ErrForeign) {
		t.Fatalf("Install over a huge hook: %v", err)
	}
	if Installed(p) {
		t.Fatal("a huge hook counts as salt's")
	}
	if b, _ := os.ReadFile(p); string(b) != big {
		t.Fatal("a huge hook was changed")
	}
}
