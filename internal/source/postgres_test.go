package source

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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
		conn, password, dbname, err := parseConn(in)
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
		if _, _, _, err := parseConn(in); !errors.Is(err, errNotPostgres) {
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
		conn, password, dbname, err := parseConn(in)
		if err != nil || [3]string{conn, password, dbname} != want {
			t.Errorf("%q: %q, %q, %q, %v; want %q", in, conn, password, dbname, err, want)
		}
	}
	for _, in := range []string{"", "   ", "memory", "dbname", "=memory", "db name=memory", "dbname='memory", "host=x dbname"} {
		if _, _, _, err := parseConn(in); !errors.Is(err, errNotPostgres) {
			t.Errorf("%q: %v, want errNotPostgres", in, err)
		}
	}
}

// libpq reads a quoted setting back unchanged.
func TestQuoteSettingRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "two words", `it's`, `back\slash`, `'\'`, "tab\there"} {
		got, rest, ok := settingValue(quoteSetting(v) + " next=1")
		if !ok || got != v || rest != " next=1" {
			t.Errorf("%q: %q, %q, %v", v, got, rest, ok)
		}
	}
}

func TestNewPostgres(t *testing.T) {
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

func TestPgDumpArgs(t *testing.T) {
	base := []string{"--no-password", "--format=plain", "--lock-wait-timeout=30000", "--file=/tmp/x/0"}
	if got, want := pgDumpArgs("postgresql://h/m", "/tmp/x/0", ""), append(slices.Clone(base), "--dbname=postgresql://h/m"); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if got, want := pgDumpArgs("dbname='m'", "/tmp/x/0", "abc"), append(slices.Clone(base), "--restrict-key=abc", "--dbname=dbname='m'"); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestPgDumpEnv(t *testing.T) {
	environ := []string{"HOME=/h", "PGPASSWORD=old"}
	got := pgDumpEnv(environ, "new")
	if want := []string{"HOME=/h", "PGPASSWORD=old", "PGPASSWORD=new", "PGCONNECT_TIMEOUT=30"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
	if environ[1] != "PGPASSWORD=old" || len(environ) != 2 {
		t.Errorf("environ changed: %q", environ)
	}
	// The person's own timeout is kept, and no password adds none.
	got = pgDumpEnv([]string{"PGCONNECT_TIMEOUT=5"}, "")
	if want := []string{"PGCONNECT_TIMEOUT=5"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}

// With pg_dump missing, the copy fails before any process starts, whether or
// not it first asks pg_dump about --restrict-key.
func TestPostgresCopyWithoutPgDump(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	db, err := NewPostgres("postgresql://localhost/memory")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "0123456789abcdef0123456789abcdef"} {
		_, err := db.Copy(context.Background(), CopyOptions{Dst: t.TempDir() + "/0", Key: key})
		if !errors.Is(err, ErrMissingProgram) || err.Error() != "salt needs the pg_dump program, which is not installed or not on PATH" {
			t.Errorf("key %q: %v", key, err)
		}
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
