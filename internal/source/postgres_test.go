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
		// As in libpq, a password may hold "?", and a user name too.
		"postgresql://agent:pa?ss@host/memory": {"postgresql://agent@host/memory", "pa?ss", "memory"},
		"postgresql://agent:pa?ss@host":        {"postgresql://agent@host", "pa?ss", ""},
		"postgresql://u?x@host/memory":         {"postgresql://u?x@host/memory", "", "memory"},
		// A setting after the host may hold "@".
		"postgresql://u:p@h?dbname=m&user=a@b": {"postgresql://u@h?dbname=m&user=a@b", "p", "m"},
		"postgresql://u:p@h/m?user=a@b":        {"postgresql://u@h/m?user=a@b", "p", "m"},
	} {
		p, err := parseURL(in)
		if err != nil || [3]string{p.conn, p.password, p.dbname} != want || !p.isURL {
			t.Errorf("%s: %q, %q, %q, %v; want %q", in, p.conn, p.password, p.dbname, err, want)
		}
	}
	for _, in := range []string{
		"mysql://agent@localhost/memory",
		"postgresql://u:%zz@localhost/memory",
		"postgresql://localhost/%zz",
		"postgresql://localhost/memory?dbname=%zz",
	} {
		if _, err := parseURL(in); !errors.Is(err, errNotPostgres) {
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
		p, err := parseSettings(in)
		if err != nil || [3]string{p.conn, p.password, p.dbname} != want || p.isURL {
			t.Errorf("%q: %q, %q, %q, %v; want %q", in, p.conn, p.password, p.dbname, err, want)
		}
	}
	for _, in := range []string{"", "   ", "memory", "dbname", "=memory", "db name=memory", "dbname='memory", "host=x dbname"} {
		if _, err := parseSettings(in); !errors.Is(err, errNotPostgres) {
			t.Errorf("%q: %v, want errNotPostgres", in, err)
		}
	}
}

// A quoted setting reads back unchanged.
func TestSettingsRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "two words", `it's`, `back\slash`, `'\'`, "tab\there"} {
		p, err := parseSettings("dbname='" + settingQuoter.Replace(v) + "'")
		if err != nil || p.dbname != v {
			t.Errorf("%q: %q, %v", v, p.dbname, err)
		}
		if again, err := parseSettings(p.conn); err != nil || again.dbname != v {
			t.Errorf("%q: written back as %q, read %q, %v", v, p.conn, again.dbname, err)
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

// A connection salt was given another way, such as by a preset, is read as
// one given to --postgres, and its errors start with where.
func TestNewPostgresConn(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	db, err := NewPostgresConn("postgresql+psycopg://agent:s3cret@localhost/memory", "the connection in DB_URL,")
	if err != nil {
		t.Fatal(err)
	}
	if db.Flag() != "--postgres" || db.Name() != "memory.sql" || db.String() != "postgresql://agent@localhost/memory" || db.(*Postgres).password != "s3cret" {
		t.Fatalf("got %q %q %q", db.Flag(), db.Name(), db.String())
	}
	_, err = NewPostgresConn("postgresql://agent:s3cret@localhost", "the connection in DB_URL,")
	if !errors.Is(err, errNoDatabase) || err.Error() != "the connection in DB_URL, "+errNoDatabase.Error() {
		t.Fatalf("no database: %v", err)
	}
	_, err = NewPostgresConn("mysql://agent:s3cret@localhost/memory", "the preset's connection")
	if !errors.Is(err, errNotPostgres) || !strings.HasPrefix(err.Error(), "the preset's connection is not a Postgres connection") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("not Postgres: %v", err)
	}
}

// The settings a connection gives are named, in the order given, without
// the password. A URL has a query only when it keeps a setting.
func TestParseKeys(t *testing.T) {
	for _, tc := range []struct {
		in       string
		keys     []string
		hasQuery bool
	}{
		{"postgresql://h/m?sslmode=require&password=x&keepalives%5Fidle=5", []string{"sslmode", "keepalives_idle"}, true},
		{"postgresql://u:x@h/m?password=y", nil, false},
		{"postgresql://u?x@h/m", nil, false},
		{"host=h password=x keepalives_count=9 dbname=m", []string{"host", "keepalives_count", "dbname"}, false},
	} {
		db, err := newPostgres("--postgres", "the connection", tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if p := db.(*Postgres); !slices.Equal(p.keys, tc.keys) || p.hasQuery != tc.hasQuery {
			t.Errorf("%q: keys %q, query %v; want %q, %v", tc.in, p.keys, p.hasQuery, tc.keys, tc.hasQuery)
		}
	}
}

// pg_dump's --help says whether it has --restrict-key, and its --version
// which libpq it has.
func TestNewPgDump(t *testing.T) {
	for _, tc := range []struct {
		help, version string
		want          pgDump
	}{
		{"  --restrict-key=RESTRICT_KEY  use provided string", "pg_dump (PostgreSQL) 17.6\n", pgDump{restrictKey: true, libpq: 17}},
		{"  --no-password  never prompt for password", "pg_dump (PostgreSQL) 11.22\n", pgDump{libpq: 11}},
		{"", "pg_dump (PostgreSQL) 18devel\n", pgDump{libpq: 18}},
		{"", "pg_dump (PostgreSQL) 17.2 (Ubuntu 17.2-1.pgdg24.04+1)\n", pgDump{libpq: 17}},
		{"", "pg_dump (PostgreSQL) 9.6.24", pgDump{libpq: 9}},
		{"", "not a version", pgDump{}},
		{"", "pg_dump (PostgreSQL) beta", pgDump{}},
		{"", "", pgDump{}},
	} {
		if got := newPgDump(tc.help, tc.version); got != tc.want {
			t.Errorf("%q, %q: %+v, want %+v", tc.help, tc.version, got, tc.want)
		}
	}
}

func TestPgDumpCommand(t *testing.T) {
	t.Setenv("PGPASSWORD", "old")
	t.Setenv("PGCONNECT_TIMEOUT", "")
	os.Unsetenv("PGCONNECT_TIMEOUT")
	t.Setenv("PGSERVICE", "")
	os.Unsetenv("PGSERVICE")
	p := &Postgres{conn: "postgresql://h/m", password: "new", isURL: true}
	o := CopyOptions{Dst: "/tmp/x/0", Key: "abc"}
	base := []string{"pg_dump", "--no-password", "--format=plain", "--lock-wait-timeout=30000", "--file=/tmp/x/0"}
	dbname := "--dbname=postgresql://h/m?keepalives_idle=30&keepalives_interval=10&keepalives_count=3"
	for d, want := range map[pgDump][]string{
		{restrictKey: true}: append(slices.Clone(base), "--restrict-key=abc", dbname),
		{}:                  append(slices.Clone(base), dbname),
	} {
		cmd := pgDumpCommand(context.Background(), p, o, d)
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
	cmd := pgDumpCommand(context.Background(), &Postgres{conn: "dbname='m'"}, o, pgDump{})
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return kv == "PGPASSWORD=" || kv == "PGCONNECT_TIMEOUT=30" }) {
		t.Errorf("env = %q", cmd.Env)
	}
}

// pg_dump gets the keepalives the connection does not set itself, with
// tcp_user_timeout only when its libpq is 12 or later.
func TestDumpConn(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	os.Unsetenv("PGSERVICE")
	const keep = "keepalives_idle=30 keepalives_interval=10 keepalives_count=3"
	const keepURL = "keepalives_idle=30&keepalives_interval=10&keepalives_count=3"
	for _, tc := range []struct {
		in    string
		libpq int
		want  string
	}{
		{"host=h dbname=m", 11, "host='h' dbname='m' " + keep},
		{"host=h dbname=m", 12, "host='h' dbname='m' " + keep + " tcp_user_timeout=60000"},
		{"host=h dbname=m", 0, "host='h' dbname='m' " + keep},
		{"postgresql://h/m", 17, "postgresql://h/m?" + keepURL + "&tcp_user_timeout=60000"},
		{"postgresql://h/m?sslmode=require", 0, "postgresql://h/m?sslmode=require&" + keepURL},
		// A "?" in a user name or password does not start the settings.
		{"postgresql://u?x@h/m", 0, "postgresql://u?x@h/m?" + keepURL},
		{"postgresql://u:pa?ss@h/m", 0, "postgresql://u@h/m?" + keepURL},
		// The connection's own settings are kept, and only the others added.
		{"postgresql://h/m?keepalives_idle=300&tcp_user_timeout=0", 17, "postgresql://h/m?keepalives_idle=300&tcp_user_timeout=0&keepalives_interval=10&keepalives_count=3"},
		{"dbname=m keepalives_idle=300 keepalives_interval=5 keepalives_count=9", 0, "dbname='m' keepalives_idle='300' keepalives_interval='5' keepalives_count='9'"},
		// Keepalives turned off stay off, with no tcp_user_timeout either.
		{"postgresql://h/m?keepalives=0", 17, "postgresql://h/m?keepalives=0"},
		{"dbname=m keepalives=0", 17, "dbname='m' keepalives='0'"},
		// A service file may set its own.
		{"service=agent dbname=m", 17, "service='agent' dbname='m'"},
		{"postgresql://h/m?service=agent", 0, "postgresql://h/m?service=agent"},
	} {
		p, err := newPostgres("--postgres", "the connection", tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.(*Postgres).dumpConn(tc.libpq); got != tc.want {
			t.Errorf("%q, %d: %q, want %q", tc.in, tc.libpq, got, tc.want)
		}
		// Messages show the connection as given.
		if strings.Contains(p.String(), "keepalives_interval=10") {
			t.Errorf("%q: shown as %q", tc.in, p.String())
		}
	}
	// PGSERVICE names a service for every connection.
	t.Setenv("PGSERVICE", "agent")
	if got := (&Postgres{conn: "dbname='m'"}).dumpConn(17); got != "dbname='m'" {
		t.Errorf("with PGSERVICE: %q", got)
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
		// Only a kind given by a connection can be made from one; a file
		// is found by its path.
		if (k.Conn != nil) != (k.Flag == "postgres") {
			t.Errorf("%s: Conn set is %v", k.Flag, k.Conn != nil)
		}
		if k.Conn != nil {
			if db, err := k.Conn(args[k.Flag], "the connection"); err != nil || db.Name() != "memory.sql" {
				t.Errorf("%s: Conn = %v, %v", k.Flag, db, err)
			}
		}
	}
}
