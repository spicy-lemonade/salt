//go:build e2e

// Package e2e runs the real salt binary. Run it with `make e2e`, which builds
// salt once and runs these tests inside a memory- and pid-capped container.
// The tests never build salt themselves.
package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func saltBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("SALT_BIN")
	if bin == "" {
		t.Fatal("SALT_BIN is not set; run e2e tests with `make e2e`")
	}
	if !filepath.IsAbs(bin) {
		t.Fatalf("SALT_BIN must be an absolute path, got %q", bin)
	}
	return bin
}

// run runs salt with a clean environment: only PATH, HOME and extra.
func run(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(saltBin(t), "version")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, extra...)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("running salt: %v", err)
		return "", -1
	}
}

func TestVersion(t *testing.T) {
	out, code := run(t)
	if code != 0 || !strings.HasPrefix(out, "salt ") {
		t.Fatalf("salt version: exit %d, output %q", code, out)
	}
}

func TestNestedSaltRefused(t *testing.T) {
	out, code := run(t, "SALT_ACTIVE=1")
	if code != 3 || !strings.Contains(out, "refusing to start a nested salt") {
		t.Fatalf("nested salt: exit %d, output %q", code, out)
	}
}
