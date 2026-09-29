package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
