package source

import (
	"context"
	"strings"
	"testing"
)

// copyRecorder is a database that records the copies asked of it.
type copyRecorder struct{ copies []CopyOptions }

func (*copyRecorder) Name() string   { return "state.db" }
func (*copyRecorder) String() string { return "~/agent/state.db" }
func (*copyRecorder) Flag() string   { return "--test" }
func (c *copyRecorder) Copy(_ context.Context, o CopyOptions) (Meta, error) {
	c.copies = append(c.copies, o)
	return Meta{Mode: 0o640}, nil
}

// A named database is backed up under the chosen name, cleaned, and is
// otherwise the database it names.
func TestNamed(t *testing.T) {
	for given, want := range map[string]string{
		"honcho.sql":          "honcho.sql",
		"agent2//state.db":    "agent2/state.db",
		"./agent2/./state.db": "agent2/state.db",
		"a b/postgres.sql":    "a b/postgres.sql",
	} {
		rec := &copyRecorder{}
		db, err := Named(rec, given)
		if err != nil {
			t.Fatalf("Named(%q): %v", given, err)
		}
		if db.Name() != want || db.String() != rec.String() || db.Flag() != rec.Flag() {
			t.Fatalf("Named(%q) = %q, %q, %q", given, db.Name(), db.String(), db.Flag())
		}
		o := CopyOptions{Dst: "/tmp/x", Key: "k"}
		if meta, err := db.Copy(context.Background(), o); err != nil || meta.Mode != 0o640 || len(rec.copies) != 1 || rec.copies[0] != o {
			t.Fatalf("Named(%q).Copy = %v, %v; copies %v", given, meta, err, rec.copies)
		}
	}
}

// A name that could lead outside the backup is refused, saying what to give
// instead. A Postgres connection named this way keeps its password out of
// the error.
func TestNamedRefusesUnsafeNames(t *testing.T) {
	pg, err := NewPostgres("postgresql://agent:hunter2@localhost/postgres")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../state.db", "a/../../state.db", "/etc/state.db", `agent\state.db`, "state\x00.db", "agent2/", "agent2/state.db/"} {
		_, err := Named(pg, name)
		if err == nil || !strings.Contains(err.Error(), "given to --name cannot be used in the backup; give a relative path such as honcho.sql") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Named(%q): %v", name, err)
		}
	}
}
