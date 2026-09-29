package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRestoreLog(t *testing.T) {
	base := t.TempDir()
	l := restoreLog{filepath.Join(base, "cache", "restores.json")}
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.Mkdir(a, 0o700)
	os.Mkdir(b, 0o700)

	if dirs, err := l.leftovers(); err != nil || len(dirs) != 0 {
		t.Fatalf("empty log: %v, %v", dirs, err)
	}
	if err := l.add(a); err != nil {
		t.Fatal(err)
	}
	if err := l.add(b); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(l.path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v, want 0600", fi.Mode().Perm())
	}
	if dirs, _ := l.leftovers(); !slices.Equal(dirs, []string{b, a}) {
		t.Fatalf("leftovers = %v", dirs)
	}
	if err := l.remove(b); err != nil {
		t.Fatal(err)
	}
	if err := l.remove(b); err != nil { // already gone from the list
		t.Fatal(err)
	}
	// A folder deleted by hand is no longer a leftover, and the next add
	// drops it from the list.
	os.Remove(a)
	if dirs, _ := l.leftovers(); len(dirs) != 0 {
		t.Fatalf("leftovers after deleting a = %v", dirs)
	}
	l.add(b)
	if dirs, _ := l.load(); !slices.Equal(dirs, []string{b}) {
		t.Fatalf("log = %v, want only %s", dirs, b)
	}

	// An unreadable log is reported, and started afresh by add.
	os.WriteFile(l.path, []byte("{"), 0o600)
	if _, err := l.leftovers(); err == nil {
		t.Fatal("corrupt log read without error")
	}
	if err := l.add(b); err != nil {
		t.Fatal(err)
	}
	if dirs, _ := l.load(); !slices.Equal(dirs, []string{b}) {
		t.Fatalf("log after a corrupt one = %v", dirs)
	}
}

func TestRestoreInterrupted(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "out")
	err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "the partly restored files were removed") {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest created: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if dirs, _ := e.app.restoreLog().load(); len(dirs) != 0 {
		t.Fatalf("restores.json still lists %v", dirs)
	}
}

func TestRestoreWarnsAboutLeftovers(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	parent := t.TempDir()
	old := filepath.Join(parent, ".salt-restore-123")
	os.Mkdir(old, 0o700)
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(parent, "out")}); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); !strings.Contains(out, old+" was left by a restore that did not finish") {
		t.Fatalf("no warning about %s:\n%s", old, out)
	}
}

// If the list can't be written, the restore still runs and says what to
// delete by hand should it be interrupted.
func TestRestoreWhenTheLogCannotBeWritten(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	e.app.CacheDir = filepath.Join(t.TempDir(), "file")
	os.WriteFile(e.app.CacheDir, nil, 0o600)
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "out")}); err != nil {
		t.Fatal(err)
	}
	if out := e.ui.out.String(); !strings.Contains(out, "could not record the restore in progress") {
		t.Fatalf("no warning:\n%s", out)
	}
}

func TestDoctorReportsLeftoverRestores(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	left, gone := filepath.Join(t.TempDir(), ".salt-restore-1"), filepath.Join(t.TempDir(), ".salt-restore-2")
	os.Mkdir(left, 0o700)
	e.app.restoreLog().save([]string{left, gone})
	e.app.Doctor(e.root)
	out := e.ui.out.String()
	if !strings.Contains(out, "! "+left+" was left by a restore that did not finish and may hold decrypted files") {
		t.Fatalf("leftover not reported:\n%s", out)
	}
	if strings.Contains(out, gone) {
		t.Fatalf("a folder that is gone was reported:\n%s", out)
	}

	os.WriteFile(e.app.restoreLog().path, []byte("{"), 0o600)
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "could not read the list of restores in progress") {
		t.Fatalf("corrupt list not reported:\n%s", e.ui.out.String())
	}
}

func TestVerifyListsAtMostFiftyProblems(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	src := filepath.Join(t.TempDir(), "hermes")
	os.MkdirAll(src, 0o755)
	for i := 0; i < 60; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.md", i)), []byte(fmt.Sprint(i)), 0o644)
	}
	if err := e.app.Seal(src, e.root, true); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(e.root, "objects"))
	e.ui.out.Reset()
	if err := e.app.Verify(e.root); !errors.Is(err, ErrReported) {
		t.Fatalf("Verify: %v", err)
	}
	out := e.ui.out.String()
	if !strings.Contains(out, "✗ 60 of 60 files cannot be restored:") || !strings.Contains(out, "  … and 10 more\n") {
		t.Fatalf("verify output:\n%s", out)
	}
	if n := strings.Count(out, "is missing"); n != 50 {
		t.Fatalf("%d problems listed, want 50", n)
	}
}
