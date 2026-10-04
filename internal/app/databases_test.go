package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/source"
)

// fakeCopy makes fake databases and records the copies they made, so tests
// can check they are removed.
type fakeCopy struct {
	made []string
	err  error
	// keys are the keys the copies were given.
	keys []string
}

// fakeDB stands in for a live database file. Its copy is a plain copy of the
// file, after which the file is re-dated, as sqlite3 can do when it closes.
type fakeDB struct {
	fc   *fakeCopy
	path string
	// copy, if set, replaces the plain copy.
	copy func(ctx context.Context, o source.CopyOptions) (source.Meta, error)
}

func (f *fakeCopy) db(path string) *fakeDB { return &fakeDB{fc: f, path: path} }

// dbs makes a fake database for each path.
func (f *fakeCopy) dbs(paths ...string) []source.Database {
	var dbs []source.Database
	for _, p := range paths {
		dbs = append(dbs, f.db(p))
	}
	return dbs
}

func (d *fakeDB) Name() string   { return filepath.Base(d.path) }
func (d *fakeDB) String() string { return d.path }
func (d *fakeDB) Flag() string   { return "--fake" }
func (d *fakeDB) Copy(ctx context.Context, o source.CopyOptions) (source.Meta, error) {
	if d.copy != nil {
		return d.copy(ctx, o)
	}
	return d.plainCopy(o)
}

func (d *fakeDB) plainCopy(o source.CopyOptions) (source.Meta, error) {
	d.fc.keys = append(d.fc.keys, o.Key)
	fi, err := os.Stat(d.path)
	if err != nil {
		return source.Meta{}, err
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		return source.Meta{}, err
	}
	if err := os.WriteFile(o.Dst, b, 0o644); err != nil {
		return source.Meta{}, err
	}
	d.fc.made = append(d.fc.made, o.Dst)
	if err := os.Chtimes(d.path, time.Time{}, time.Now()); err != nil {
		return source.Meta{}, err
	}
	return source.Meta{Mode: fi.Mode(), ModTime: fi.ModTime()}, d.fc.err
}

// sqliteEnv is a healthy repo with an empty source folder, a private temp
// folder, a fake copier and one live database.
func sqliteEnv(t *testing.T) (e *testEnv, fc *fakeCopy, src, db string) {
	t.Helper()
	e = newEnv(t)
	healthyRepo(t, e)
	t.Setenv("TMPDIR", t.TempDir())
	fc = &fakeCopy{}
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

// named gives db another name, failing the test if the name is refused.
func named(t *testing.T, db source.Database, name string) source.Database {
	t.Helper()
	d, err := source.Named(db, name)
	if err != nil {
		t.Fatal(err)
	}
	return d
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
func TestSealCopiesDatabase(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	at := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	os.Chtimes(db, time.Time{}, at)
	os.WriteFile(filepath.Join(src, "SOUL.md"), []byte("be kind"), 0o644)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: fc.dbs(db)}); err != nil {
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

// Every way a copy can fail stops the seal before anything is written, names
// the database, and removes the copies already made.
func TestSealDatabaseCopyFails(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		dbs  func(db string) []string
		want string
	}{
		"copy fails": {errors.New("database disk image is malformed"),
			func(db string) []string { return []string{db} }, "copying the database ~/agent/memory.db: database disk image is malformed"},
		"second missing": {nil,
			func(db string) []string { return []string{db, filepath.Join(filepath.Dir(db), "gone.db")} },
			"the database ~/agent/gone.db does not exist; check the path given to --fake"},
		"vanishes during copy": {fs.ErrNotExist,
			func(db string) []string { return []string{db} }, "the database ~/agent/memory.db does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			e, fc, src, db := sqliteEnv(t)
			fc.err = tc.err
			e.ui.out.Reset()
			err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: fc.dbs(tc.dbs(db)...)})
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
func TestSealDatabaseRemovesCopiesWhenSealFails(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	err := e.app.Seal(SealOptions{Src: filepath.Join(src, "missing"), Repo: e.root, Databases: fc.dbs(db)})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// Two databases whose names clash, the same name, one a folder above the
// other, or names that differ only in case, are refused before any copy is
// made, naming both and saying how to give one another name.
func TestSealDatabaseNameClash(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	other := filepath.Join(e.app.Home, "agent2", "memory.db")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("SQLite format 3\x00"), 0o644)
	for name, tc := range map[string]struct {
		dbs  []source.Database
		want string
	}{
		"two databases": {[]source.Database{fc.db(db), fc.db(other)},
			"the databases ~/agent/memory.db and ~/agent2/memory.db would both be backed up as memory.db. Give one of them another name with --name NAME before its --fake"},
		"same db, twice": {fc.dbs(db, db), "the database ~/agent/memory.db is given twice. Give it once"},
		"chosen name":    {[]source.Database{fc.db(db), named(t, fc.db(other), "memory.db")}, "would both be backed up as memory.db"},
		"folder above": {[]source.Database{fc.db(db), named(t, fc.db(other), "memory.db/agent2.db")},
			"the databases ~/agent/memory.db and ~/agent2/memory.db would be backed up as memory.db and memory.db/agent2.db, which clash because a file cannot also be a folder. Give one"},
	} {
		err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: tc.dbs})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(fc.made) != 0 {
		t.Fatalf("copies made before refusing: %v", fc.made)
	}
	assertNoCopiesLeft(t, fc)
}

// A database whose name, its own or a chosen one, is a file or folder in the
// source is refused with advice.
func TestSealDatabaseSourceClash(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	os.WriteFile(filepath.Join(src, "memory.db"), []byte("old copy"), 0o644)
	os.MkdirAll(filepath.Join(src, "agent"), 0o755)
	os.WriteFile(filepath.Join(src, "agent", "SOUL.md"), []byte("be kind"), 0o644)
	for _, name := range []string{"", "agent", "agent/SOUL.md", "memory.db/state.db"} {
		d := source.Database(fc.db(db))
		if name != "" {
			d = named(t, d, name)
		}
		err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: []source.Database{d}})
		if !errors.Is(err, seal.ErrDuplicatePath) || !strings.Contains(err.Error(), "each database salt copies is backed up under its own name") ||
			!strings.Contains(err.Error(), "Give the database another name with --name NAME before its option") {
			t.Errorf("named %q: %v", name, err)
		}
	}
	assertNoCopiesLeft(t, fc)
}

// Two databases with the same name are both backed up and restored when one
// is given another name, also inside a source folder beside other files.
func TestSealNamedDatabases(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	os.MkdirAll(filepath.Join(src, "agent2"), 0o755)
	os.WriteFile(filepath.Join(src, "agent2", "SOUL.md"), []byte("be brave"), 0o644)
	other := filepath.Join(e.app.Home, "agent2", "memory.db")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("SQLite format 3\x00other memories"), 0o600)
	want := map[string]string{
		"memory.db":           "SQLite format 3\x00memories",
		"agent2/memory.db":    "SQLite format 3\x00other memories",
		"agent2/SOUL.md":      "be brave",
		"copies/again/one.db": "SQLite format 3\x00memories",
	}
	dbs := []source.Database{fc.db(db), named(t, fc.db(other), "agent2/memory.db"), named(t, fc.db(db), "copies/again/one.db")}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: dbs}); err != nil {
		t.Fatal(err)
	}
	assertNoCopiesLeft(t, fc)
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		t.Fatal(err)
	}
	for rel, content := range want {
		if b, err := os.ReadFile(filepath.Join(dest, rel)); err != nil || string(b) != content {
			t.Errorf("restored %s = %q, %v; want %q", rel, b, err, content)
		}
	}
}

// Stopping salt (Ctrl-C or SIGTERM) during a copy removes the copies and
// seals nothing.
func TestSealDatabaseInterrupted(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	d := fc.db(db)
	d.copy = func(ctx context.Context, o source.CopyOptions) (source.Meta, error) {
		d.plainCopy(o)
		cancel()
		return source.Meta{}, ctx.Err()
	}
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: []source.Database{d}, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "seal interrupted") {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// Ctrl-C can stop the copying program before salt cancels its context. The
// program's context.Canceled still counts as an interruption.
func TestSealDatabaseStoppedBeforeSaltIs(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	d := fc.db(db)
	d.copy = func(_ context.Context, o source.CopyOptions) (source.Meta, error) {
		d.plainCopy(o)
		return source.Meta{}, context.Canceled
	}
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: []source.Database{d}, Context: context.Background()})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "seal interrupted") {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// A signal after the copies are made lets the seal finish and remove them,
// but still fails, so the backup script does not go on to commit.
func TestSealDatabaseInterruptedWhileSealing(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	d := fc.db(db)
	d.copy = func(_ context.Context, o source.CopyOptions) (source.Meta, error) {
		defer cancel()
		return d.plainCopy(o)
	}
	e.ui.out.Reset()
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: []source.Database{d}, Context: ctx})
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "the backup was sealed") {
		t.Fatalf("Seal: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}

// Without databases, no copy key is made and no temp folder either.
func TestSealWithoutDatabasesMakesNoCopies(t *testing.T) {
	e, fc, src, _ := sqliteEnv(t)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	assertNoCopiesLeft(t, fc)
	if keys, _ := filepath.Glob(filepath.Join(e.app.CacheDir, "copykey-*")); len(keys) != 0 {
		t.Fatalf("made a copy key: %v", keys)
	}
}

// A temp folder that cannot be made stops the seal.
func TestSealDatabaseNoTempFolder(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: fc.dbs(db)}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal: %v", err)
	}
}

// A copy key that cannot be saved stops the seal before any copy is made.
func TestSealDatabaseNoCopyKey(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	e.app.CacheDir = filepath.Join(t.TempDir(), "file")
	os.WriteFile(e.app.CacheDir, nil, 0o600)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: fc.dbs(db)}); err == nil {
		t.Fatal("Seal succeeded without a copy key")
	}
	if len(fc.made) != 0 {
		t.Fatalf("copies made: %v", fc.made)
	}
	assertNoCopiesLeft(t, fc)
}

// Every copy gets the repo's copy key, the same on every seal, so a dump
// that would hold a random key stays the same while the database does.
func TestSealDatabaseCopyKeyIsStable(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	other := filepath.Join(filepath.Dir(db), "other.db")
	os.WriteFile(other, []byte("other"), 0o600)
	for range 2 {
		if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: fc.dbs(db, other)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fc.keys) != 4 || fc.keys[0] == "" || slices.ContainsFunc(fc.keys, func(k string) bool { return k != fc.keys[0] }) {
		t.Fatalf("copy keys = %q, want one key for all", fc.keys)
	}
}

// A database whose copy records fixed permissions and no date, as a dump
// does, is unchanged on the next seal when its contents are, and restores
// dated when restored.
func TestSealUnchangedDumpIsReused(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	d := fc.db(db)
	d.copy = func(_ context.Context, o source.CopyOptions) (source.Meta, error) {
		d.plainCopy(o)
		return source.Meta{Mode: 0o600}, nil
	}
	for i, want := range []string{"1 encrypted, 0 unchanged", "0 encrypted, 1 unchanged"} {
		e.ui.out.Reset()
		if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: []source.Database{d}}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.ui.out.String(), want) {
			t.Fatalf("seal %d: %q, want %q", i+1, e.ui.out.String(), want)
		}
	}
	before := time.Now().Add(-time.Second)
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dest, "memory.db"))
	if err != nil || fi.ModTime().Before(before) {
		t.Fatalf("restored dump: %v, %v; want dated when restored", fi, err)
	}
	assertNoCopiesLeft(t, fc)
}

// Names that differ only by case are two names in a repo with encrypted
// paths, so both databases are sealed. With --plain-paths each name is also
// a file name in the repo, which macOS and Windows would not tell apart, so
// they are refused before any copy is made, or, against a source file, once
// seal reads the source, saying why. Two files in folders whose names differ
// only by case are not refused, since they keep their own names.
func TestSealDatabaseNamesDifferingByCase(t *testing.T) {
	e, fc, src, db := sqliteEnv(t)
	os.MkdirAll(filepath.Join(src, "agent2"), 0o755)
	os.WriteFile(filepath.Join(src, "agent2", "SOUL.md"), []byte("be brave"), 0o644)
	dbs := []source.Database{fc.db(db), named(t, fc.db(db), "Memory.DB"), named(t, fc.db(db), "AGENT2/memory.db")}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Databases: dbs}); err != nil {
		t.Fatalf("encrypted paths: %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "sealed 4 files") {
		t.Fatalf("output = %q", e.ui.out.String())
	}

	plain := newEnv(t)
	plain.ui.answer = phraseAnswers(0)
	if err := plain.app.Init(InitOptions{Repo: plain.root, PlainPaths: true}); err != nil {
		t.Fatal(err)
	}
	fc = &fakeCopy{}
	err := plain.app.Seal(SealOptions{Src: src, Repo: plain.root, Databases: []source.Database{fc.db(db), named(t, fc.db(db), "Memory.DB")}})
	if err == nil || !strings.Contains(err.Error(), "would be backed up as memory.db and Memory.DB, which clash because they differ only by case, and with --plain-paths the repo would keep them as one file on macOS and Windows. Give one of them another name with --name NAME before its --fake") {
		t.Fatalf("plain paths, two databases: %v", err)
	}
	if len(fc.made) != 0 {
		t.Fatalf("copies made before refusing: %v", fc.made)
	}
	err = plain.app.Seal(SealOptions{Src: src, Repo: plain.root, Databases: []source.Database{named(t, fc.db(db), "Agent2/soul.md")}})
	if !errors.Is(err, seal.ErrDuplicatePath) || !strings.Contains(err.Error(), "agent2/SOUL.md and Agent2/soul.md differ only by case") ||
		!strings.Contains(err.Error(), "Give the database another name with --name NAME before its option") {
		t.Fatalf("plain paths, source file: %v", err)
	}
	// Files with their own names in folders that differ only by case are
	// still two files, which macOS and Windows keep in one folder.
	if err := plain.app.Seal(SealOptions{Src: src, Repo: plain.root, Databases: []source.Database{named(t, fc.db(db), "AGENT2/memory.db")}}); err != nil {
		t.Fatalf("plain paths, beside a source file: %v", err)
	}
	assertNoCopiesLeft(t, fc)
}
