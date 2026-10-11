package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestRestoreLog(t *testing.T) {
	base := t.TempDir()
	l := restoreLog{dir: filepath.Join(base, "cache", "restores"), older: filepath.Join(base, "cache", "restores.json")}
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
	if fi, _ := os.Stat(l.record(a)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v, want 0600", fi.Mode().Perm())
	}
	if dirs, _ := l.leftovers(); !slices.Equal(dirs, []string{a, b}) {
		t.Fatalf("leftovers = %v", dirs)
	}
	if err := l.remove(b); err != nil {
		t.Fatal(err)
	}
	if err := l.remove(b); err != nil { // already forgotten
		t.Fatal(err)
	}
	// A folder deleted by hand is no longer a leftover, and the next add
	// removes its record.
	os.Remove(a)
	if dirs, _ := l.leftovers(); len(dirs) != 0 {
		t.Fatalf("leftovers after deleting a = %v", dirs)
	}
	l.add(b)
	if records, _ := l.records(); len(records) != 1 || records[l.record(b)] != b {
		t.Fatalf("records = %v, want only %s", records, b)
	}

	// A record being written is not read, and one that cannot be read is
	// reported, with the folders the others name.
	os.WriteFile(filepath.Join(l.dir, "x.tmp-1"), []byte("half"), 0o600)
	os.Mkdir(filepath.Join(l.dir, "unreadable"), 0o700)
	if dirs, err := l.leftovers(); err == nil || !slices.Equal(dirs, []string{b}) {
		t.Fatalf("leftovers with an unreadable record = %v, %v", dirs, err)
	}
	os.Remove(filepath.Join(l.dir, "unreadable"))

	// The list an older salt kept is read, and moved into records by add.
	c := filepath.Join(base, "c")
	os.Mkdir(c, 0o700)
	os.WriteFile(l.older, []byte(`["`+c+`", "`+filepath.Join(base, "gone")+`"]`), 0o600)
	if dirs, err := l.leftovers(); err != nil || !slices.Equal(dirs, []string{b, c}) {
		t.Fatalf("leftovers with an older list = %v, %v", dirs, err)
	}
	if err := l.add(b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.older); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("older list kept: %v", err)
	}
	if dirs, _ := l.leftovers(); !slices.Equal(dirs, []string{b, c}) {
		t.Fatalf("leftovers after moving the older list = %v", dirs)
	}

	// An unreadable older list is reported, and dropped by add.
	os.WriteFile(l.older, []byte("{"), 0o600)
	if _, err := l.leftovers(); err == nil {
		t.Fatal("corrupt list read without error")
	}
	if err := l.add(b); err != nil {
		t.Fatal(err)
	}
	if _, err := l.leftovers(); err != nil {
		t.Fatalf("after add: %v", err)
	}

	// A records folder or an older list that is not what salt writes, such
	// as a file in place of the folder, is reported.
	broken := restoreLog{dir: filepath.Join(base, "c-file"), older: filepath.Join(base, "a-folder")}
	os.WriteFile(broken.dir, nil, 0o600)
	os.Mkdir(broken.older, 0o700)
	if _, err := broken.leftovers(); err == nil || !strings.Contains(err.Error(), "c-file") || !strings.Contains(err.Error(), "a-folder") {
		t.Fatalf("leftovers with a file for the folder and a folder for the list: %v", err)
	}
}

// Restores running at once each keep their own record, however their adds
// and removes fall, so none is lost. With one list for them all, each add
// rewrote the others' records from what it had read.
func TestRestoreLogConcurrent(t *testing.T) {
	base := t.TempDir()
	l := restoreLog{dir: filepath.Join(base, "restores"), older: filepath.Join(base, "restores.json")}
	var dirs []string
	for i := range 20 {
		d := filepath.Join(base, fmt.Sprint("r", i))
		os.Mkdir(d, 0o700)
		dirs = append(dirs, d)
	}
	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Go(func() {
			if err := l.add(d); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := l.leftovers()
	slices.Sort(dirs)
	if err != nil || !slices.Equal(got, dirs) {
		t.Fatalf("leftovers = %v, %v, want all %d", got, err, len(dirs))
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
	if records, _ := e.app.restoreLog().records(); len(records) != 0 {
		t.Fatalf("restores still recorded: %v", records)
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
	l := e.app.restoreLog()
	l.add(left)
	os.WriteFile(l.older, []byte(`["`+gone+`"]`), 0o600)
	e.app.Doctor(e.root)
	out := e.ui.out.String()
	if !strings.Contains(out, "! "+left+" was left by a restore that did not finish and may hold decrypted files") {
		t.Fatalf("leftover not reported:\n%s", out)
	}
	if strings.Contains(out, gone) {
		t.Fatalf("a folder that is gone was reported:\n%s", out)
	}

	os.WriteFile(l.older, []byte("{"), 0o600)
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "could not read the list of restores in progress") {
		t.Fatalf("corrupt list not reported:\n%s", e.ui.out.String())
	}
}

func TestVerifyListsAtMostFiftyProblems(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	src := filepath.Join(t.TempDir(), "agent")
	os.MkdirAll(src, 0o755)
	for i := 0; i < 60; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.md", i)), []byte(fmt.Sprint(i)), 0o644)
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(e.root, "objects"))
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, false); !errors.Is(err, ErrReported) {
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
