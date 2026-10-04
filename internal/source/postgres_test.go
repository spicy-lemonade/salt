package source

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/proc"
)

func TestParseURL(t *testing.T) {
	for in, want := range map[string][3]string{
		"postgresql://agent:s3cret@localhost:5432/memory":                   {"postgresql://agent@localhost:5432/memory", "s3cret", "memory"},
		"postgres://agent@localhost/memory":                                 {"postgresql://agent@localhost/memory", "", "memory"},
		"postgresql+psycopg://postgres:postgres@localhost:5432/postgres":    {"postgresql://postgres@localhost:5432/postgres", "postgres", "postgres"},
		"postgresql+asyncpg://u:p%40ss%3Aw0rd@db.internal/agent%20memory":   {"postgresql://u@db.internal/agent%20memory", "p@ss:w0rd", "agent memory"},
		"postgresql:///memory?host=/var/run/postgresql&password=x&sslmode=": {"postgresql:///memory?host=/var/run/postgresql&sslmode=", "x", "memory"},
		"postgresql://h1:5432,h2:5433/memory?target_session_attrs=any":      {"postgresql://h1:5432,h2:5433/memory?target_session_attrs=any", "", "memory"},
		"postgresql://localhost?dbname=memory&&connect_timeout=5":           {"postgresql://localhost?dbname=memory&connect_timeout=5", "", "memory"},
		"postgresql://us:er:pa@ss@localhost/memory":                         {"postgresql://us@localhost/memory", "er:pa@ss", "memory"},
		"postgresql://agent:@localhost/memory":                              {"postgresql://agent@localhost/memory", "", "memory"},
		"postgresql://%2Fvar%2Frun%2Fpostgresql/memory":                     {"postgresql://%2Fvar%2Frun%2Fpostgresql/memory", "", "memory"},
		"postgresql://localhost":                                            {"postgresql://localhost", "", ""},
	} {
		conn, password, dbname, err := parseURL(in)
		if err != nil || [3]string{conn, password, dbname} != want {
			t.Errorf("%s: %q, %q, %q, %v; want %q", in, conn, password, dbname, err, want)
		}
	}
	for _, in := range []string{
		"mysql://agent@localhost/memory",
		"postgresql://u:%zz@localhost/memory",
		"postgresql://localhost/%zz",
		"postgresql://localhost/memory?dbname=%zz",
	} {
		if _, _, _, err := parseURL(in); !errors.Is(err, errNotPostgres) {
			t.Errorf("%s: %v, want errNotPostgres", in, err)
		}
	}
}

func TestParseSettings(t *testing.T) {
	for in, want := range map[string][3]string{
		"host=localhost dbname=memory":                             {"host='localhost' dbname='memory'", "", "memory"},
		"  host = localhost\tport=5432  password=s3cret dbname=m ": {"host='localhost' port='5432' dbname='m'", "s3cret", "m"},
		`dbname='agent memory' password='it\'s \\ here'`:           {`dbname='agent memory'`, `it's \ here`, "agent memory"},
		`dbname=a\ b user=''`:                                      {`dbname='a b' user=''`, "", "a b"},
		`dbname=it's`:                                              {`dbname='it\'s'`, "", "it's"},
		"user=agent":                                               {"user='agent'", "", ""},
		"dbname=postgresql://h/x":                                  {"dbname='postgresql://h/x'", "", "postgresql://h/x"},
	} {
		conn, password, dbname, err := parseSettings(in)
		if err != nil || [3]string{conn, password, dbname} != want {
			t.Errorf("%q: %q, %q, %q, %v; want %q", in, conn, password, dbname, err, want)
		}
	}
	for _, in := range []string{"", "   ", "memory", "dbname", "=memory", "db name=memory", "dbname='memory", "host=x dbname"} {
		if _, _, _, err := parseSettings(in); !errors.Is(err, errNotPostgres) {
			t.Errorf("%q: %v, want errNotPostgres", in, err)
		}
	}
}

// A quoted setting reads back unchanged.
func TestSettingsRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "two words", `it's`, `back\slash`, `'\'`, "tab\there"} {
		conn, _, dbname, err := parseSettings("dbname='" + settingQuoter.Replace(v) + "'")
		if err != nil || dbname != v {
			t.Errorf("%q: %q, %v", v, dbname, err)
		}
		if _, _, again, err := parseSettings(conn); err != nil || again != v {
			t.Errorf("%q: written back as %q, read %q, %v", v, conn, again, err)
		}
	}
}

func TestNewPostgres(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	db, err := NewPostgres("postgresql+psycopg://agent:s3cret@localhost:5432/memory")
	if err != nil {
		t.Fatal(err)
	}
	p := db.(*Postgres)
	if p.Name() != "memory.sql" || p.String() != "postgresql://agent@localhost:5432/memory" || p.Flag() != "--postgres" || p.password != "s3cret" {
		t.Fatalf("got %q %q %q %q", p.Name(), p.String(), p.Flag(), p.password)
	}
	for in, want := range map[string]error{
		"postgresql://agent:s3cret@localhost":        errNoDatabase,
		"host=localhost password=s3cret":             errNoDatabase,
		"postgresql://agent:s3cret@localhost/a%2Fb":  errBadName,
		"postgresql://agent:s3cret@localhost/..":     errBadName,
		"postgresql://agent:s3cret@localhost/.":      errBadName,
		`dbname=a\\b password=s3cret`:                errBadName,
		"postgresql://agent:s3cret@localhost/a%0Ab":  errBadName,
		"postgresql://agent:s3cret@localhost/a%7Fb":  errBadName,
		"mysql://agent:s3cret@localhost/memory":      errNotPostgres,
		"postgresql://agent:s3cret@localhost/m?a=%z": errNotPostgres,
	} {
		_, err := NewPostgres(in)
		if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "the connection given to --postgres ") {
			t.Errorf("%s: %v, want %v", in, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: the error shows the password: %v", in, err)
		}
	}
}

func TestNewPostgresEnv(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	t.Setenv("SALT_TEST_DB", "postgresql://agent:s3cret@localhost/memory")
	db, err := NewPostgresEnv("SALT_TEST_DB")
	if err != nil {
		t.Fatal(err)
	}
	if db.Flag() != "--postgres-env" || db.Name() != "memory.sql" || db.(*Postgres).password != "s3cret" {
		t.Fatalf("got %q %q", db.Flag(), db.Name())
	}
	t.Setenv("SALT_TEST_EMPTY", "")
	for name, want := range map[string]string{
		"SALT_TEST_UNSET": "the environment variable SALT_TEST_UNSET given to --postgres-env is not set or is empty",
		"SALT_TEST_EMPTY": "the environment variable SALT_TEST_EMPTY given to --postgres-env is not set or is empty",
	} {
		if _, err := NewPostgresEnv(name); err == nil || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
	t.Setenv("SALT_TEST_DB", "postgresql://agent:s3cret@localhost")
	_, err = NewPostgresEnv("SALT_TEST_DB")
	if !errors.Is(err, errNoDatabase) || !strings.HasPrefix(err.Error(), "the connection in SALT_TEST_DB, given to --postgres-env, names no database") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("no database: %v", err)
	}
}

func TestPgDumpCommand(t *testing.T) {
	t.Setenv("PGPASSWORD", "old")
	t.Setenv("PGCONNECT_TIMEOUT", "")
	os.Unsetenv("PGCONNECT_TIMEOUT")
	p := &Postgres{conn: "postgresql://h/m", password: "new"}
	o := CopyOptions{Dst: "/tmp/x/0", Key: "abc"}
	base := []string{"pg_dump", "--no-password", "--format=plain", "--lock-wait-timeout=30000", "--file=/tmp/x/0"}
	for help, want := range map[string][]string{
		"  --restrict-key=RESTRICT_KEY  use provided string": append(slices.Clone(base), "--restrict-key=abc", "--dbname=postgresql://h/m"),
		"  --no-password  never prompt for password":         append(slices.Clone(base), "--dbname=postgresql://h/m"),
	} {
		cmd := pgDumpCommand(context.Background(), p, o, help)
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("args = %q, want %q", cmd.Args, want)
		}
		// The connection's password wins, as it comes last.
		env := cmd.Env[len(cmd.Env)-2:]
		if want := []string{"PGPASSWORD=new", "PGCONNECT_TIMEOUT=30"}; !slices.Equal(env, want) {
			t.Errorf("env ends %q, want %q", env, want)
		}
	}
	// The person's own timeout is kept, and no password adds none.
	t.Setenv("PGCONNECT_TIMEOUT", "5")
	cmd := pgDumpCommand(context.Background(), &Postgres{conn: "dbname='m'"}, o, "")
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return kv == "PGPASSWORD=" || kv == "PGCONNECT_TIMEOUT=30" }) {
		t.Errorf("env = %q", cmd.Env)
	}
}

// A connection without a database name uses PGDATABASE, as libpq does.
func TestNewPostgresUsesPGDATABASE(t *testing.T) {
	t.Setenv("PGDATABASE", "memory")
	db, err := NewPostgres("host=localhost")
	if err != nil || db.Name() != "memory.sql" {
		t.Fatalf("NewPostgres = %v, %v", db, err)
	}
	t.Setenv("PGDATABASE", "")
	if _, err := NewPostgres("host=localhost"); !errors.Is(err, errNoDatabase) {
		t.Fatalf("without PGDATABASE: %v", err)
	}
}

// With pg_dump missing, the copy fails before any process starts.
func TestPostgresCopyWithoutPgDump(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	db, err := NewPostgres("postgresql://localhost/memory")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Copy(context.Background(), CopyOptions{Dst: t.TempDir() + "/0", Key: "abc"})
	if !errors.Is(err, proc.ErrMissingProgram) || err.Error() != "salt needs the pg_dump program, which is not installed or not on PATH" {
		t.Fatalf("Copy: %v", err)
	}
}

// Each kind's option makes a database of that kind.
func TestKinds(t *testing.T) {
	t.Setenv("SALT_TEST_DB", "dbname=memory")
	args := map[string]string{"sqlite": "memory.db", "postgres": "dbname=memory", "postgres-env": "SALT_TEST_DB"}
	for _, k := range Kinds {
		db, err := k.New(args[k.Flag])
		if err != nil {
			t.Fatalf("%s: %v", k.Flag, err)
		}
		if db.Flag() != "--"+k.Flag || k.Usage == "" {
			t.Errorf("%s: flag %q, usage %q", k.Flag, db.Flag(), k.Usage)
		}
	}
}
