package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCopy stands in for source.CopySQLite. It copies the file as sqlite3
// would, then re-dates the live database, as sqlite3 can when it closes. It
// records the copies it made so tests can check they are removed.
type fakeCopy struct {
	made []string
	err  error
}

func (f *fakeCopy) copy(_ context.Context, live, dst string) error {
	b, err := os.ReadFile(live)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	f.made = append(f.made, dst)
	if err := os.Chtimes(live, time.Time{}, time.Now()); err != nil {
		return err
	}
	return f.err
}

// sqliteEnv is a healthy repo with an empty source folder, a private temp
// folder, a fake copier and one live database.
func sqliteEnv(t *testing.T) (e *testEnv, fc *fakeCopy, src, db string) {
	t.Helper()
	e = newEnv(t)
	healthyRepo(t, e)
	t.Setenv("TMPDIR", t.TempDir())
	fc = &fakeCopy{}
	e.app.CopySQLite = fc.copy
	src = filepath.Join(t.TempDir(), "stage")
	os.MkdirAll(src, 0o755)
	// Messages show the database inside home as ~/agent/memory.db.
	e.app.Home = t.TempDir()
	db = filepath.Join(e.app.Home, "agent", "memory.db")
	os.MkdirAll(filepath.Dir(db), 0o755)
	if err := os.WriteFile(db, []byte("SQLite format 3\x00memories"), 0o640); err != nil {
		t.Fatal(err)
	}
	return e, fc, src, db
}

// assertNoCopiesLeft checks every copy, and the temp folder, is gone.
func assertNoCopiesLeft(t *testing.T, fc *fakeCopy) {
	t.Helper()
	if left, _ := os.ReadDir(os.Getenv("TMPDIR")); len(left) != 0 {
		t.Fatalf("left in the temp folder: %v", left)
	}
	for _, p := range fc.made {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("copy %s was not removed: %v", p, err)
		}
	}
}

// A database is copied, sealed at the top of the backup with the live
// file's date from before the copy, and the copy is removed.
func TestSealCopiesSQLite(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	at := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	os.Chtimes(db, time.Time{}, at)
	os.WriteFile(filepath.Join(src, "SOUL.md"), []byte("be kind"), 0o644)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: []string{db}}); err != nil {
		t.Fatal(err)
	}
	if len(fc.made) != 1 {
		t.Fatalf("copies made: %v", fc.made)
	}
	assertNoCopiesLeft(t, fc)
	if !strings.Contains(e.ui.out.String(), "sealed 2 files") {
		t.Fatalf("output = %q", e.ui.out.String())
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dest, "memory.db")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "SQLite format 3\x00memories" {
		t.Fatalf("restored database = %q, %v", b, err)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(at) {
		t.Fatalf("restored database is dated %v, want %v", fi.ModTime(), at)
	}
}

// A relative database path is resolved from the current folder, and the
// copier gets it absolute, since sqlite3 runs in the temp folder.
func TestSealSQLiteRelativePath(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	t.Chdir(filepath.Dir(db))
	var got string
	e.app.CopySQLite = func(ctx context.Context, live, dst string) error {
		got = live
		return fc.copy(ctx, live, dst)
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: []string{"memory.db"}}); err != nil {
		t.Fatal(err)
	}
	if got != db {
		t.Fatalf("copier got %q, want %q", got, db)
	}
	assertNoCopiesLeft(t, fc)
}

// Every way a copy can fail stops the seal before anything is written, names
// the database, and removes the copies already made.
func TestSealSQLiteCopyFails(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		dbs  func(db string) []string
		want string
	}{
		"copy fails": {errors.New("database disk image is malformed"),
			func(db string) []string { return []string{db} }, "copying the database ~/agent/memory.db: database disk image is malformed"},
		"second missing": {nil,
			func(db string) []string { return []string{db, filepath.Join(filepath.Dir(db), "gone.db")} },
			"the database ~/agent/gone.db does not exist; check the path given to --sqlite"},
		"vanishes during copy": {fs.ErrNotExist,
			func(db string) []string { return []string{db} }, "the database ~/agent/memory.db does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			e, fc, src, db := sqliteEnv(t)
			fc.err = tc.err
			e.ui.out.Reset()
			err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: tc.dbs(db)})
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Seal: %v", err)
			}
			assertNoCopiesLeft(t, fc)
			if strings.Contains(e.ui.out.String(), "sealed") {
				t.Fatalf("a failed copy still sealed:\n%s", e.ui.out.String())
			}
		})
	}
}

// The copies are removed when sealing itself fails, too.
func TestSealSQLiteRemovesCopiesWhenSealFails(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	err := e.app.Seal(SealOptions{Src: filepath.Join(src, "missing"), Repo: e.root, SQLite: []string{db}})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// Two databases, or a database and a top-level source file, with the same
// name are refused with advice.
func TestSealSQLiteNameClash(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	other := filepath.Join(t.TempDir(), "memory.db")
	os.WriteFile(other, []byte("SQLite format 3\x00"), 0o644)
	os.WriteFile(filepath.Join(src, "state.db"), []byte("old copy"), 0o644)
	state := filepath.Join(filepath.Dir(db), "state.db")
	os.WriteFile(state, []byte("SQLite format 3\x00"), 0o644)
	for name, dbs := range map[string][]string{
		"two databases":  {db, other},
		"source file":    {state},
		"same db, twice": {db, db},
	} {
		err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: dbs})
		if err == nil || !strings.Contains(err.Error(), "each --sqlite database is backed up under its file name") {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertNoCopiesLeft(t, fc)
}

// Stopping salt (Ctrl-C or SIGTERM) during a copy removes the copies and
// seals nothing.
func TestSealSQLiteInterrupted(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.app.CopySQLite = func(ctx context.Context, live, dst string) error {
		fc.copy(ctx, live, dst)
		cancel()
		return ctx.Err()
	}
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: []string{db}, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "seal interrupted") {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// A signal after the copies are made lets the seal finish and remove them,
// but still fails, so the backup script does not go on to commit.
func TestSealSQLiteInterruptedWhileSealing(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.app.CopySQLite = func(ctx context.Context, live, dst string) error {
		defer cancel()
		return fc.copy(ctx, live, dst)
	}
	e.ui.out.Reset()
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: []string{db}, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "the backup was sealed") {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// Without databases, the copier is never called and no temp folder is made.
func TestSealWithoutSQLiteMakesNoCopies(t *testing.T) {
	e, fc, src, _ := sqliteEnv(t)
	e.app.CopySQLite = nil // would panic if called
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	assertNoCopiesLeft(t, fc)
}

// A temp folder that cannot be made stops the seal.
func TestSealSQLiteNoTempFolder(t *testing.T) {
	e, _, src, db := sqliteEnv(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, SQLite: []string{db}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal: %v", err)
	}
}
