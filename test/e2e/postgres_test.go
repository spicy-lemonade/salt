//go:build e2e

package e2e

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pgPassword is the password of the test server's only user, agent.
const pgPassword = "pa:ss@w0rd s3cret"

// pgServer is a private Postgres server for one test, reached over TCP on
// 127.0.0.1 with a password, as an agent reaches its database.
type pgServer struct {
	bin, data string
	port      int
	// e runs the server's programs as agent without asking for a password.
	e *env
}

// startPostgres starts a new server in a temporary folder and stops it when
// the test ends. It has no Unix socket, so every connection uses TCP and the
// password. It skips the test when Postgres is not installed.
func startPostgres(t *testing.T, e *env) *pgServer {
	t.Helper()
	// Debian and Ubuntu keep Postgres's programs out of PATH, one folder per
	// version.
	bins, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	slices.SortFunc(bins, func(a, b string) int {
		va, _ := strconv.Atoi(filepath.Base(filepath.Dir(a)))
		vb, _ := strconv.Atoi(filepath.Base(filepath.Dir(b)))
		return vb - va
	})
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		bins = append([]string{filepath.Dir(p)}, bins...)
	}
	if len(bins) == 0 {
		t.Skip("Postgres is not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	base := t.TempDir()
	s := &pgServer{bin: bins[0], data: filepath.Join(base, "data"), port: port}
	// Without a locale, Postgres on macOS refuses to start.
	e = e.with("LC_ALL=C")
	pw := filepath.Join(base, "pw")
	write(t, pw, pgPassword+"\n")
	e.must(base, filepath.Join(s.bin, "initdb"), "-D", s.data, "-U", "agent", "--pwfile", pw, "--auth", "scram-sha-256", "-E", "UTF8", "--no-locale")
	// Stopped even if starting fails part way.
	t.Cleanup(s.stop)
	opts := "-c listen_addresses=127.0.0.1 -c unix_socket_directories='' -c port=" + strconv.Itoa(port)
	if out, code := e.run(base, filepath.Join(s.bin, "pg_ctl"), "-D", s.data, "-o", opts, "-l", filepath.Join(base, "log"), "-w", "-t", "30", "start"); code != 0 {
		log, _ := os.ReadFile(filepath.Join(base, "log"))
		t.Fatalf("the Postgres server did not start: %s\n%s", out, log)
	}
	s.e = e.with("PGHOST=127.0.0.1", "PGPORT="+strconv.Itoa(port), "PGUSER=agent", "PGPASSWORD="+pgPassword)
	return s
}

func (s *pgServer) stop() {
	exec.Command(filepath.Join(s.bin, "pg_ctl"), "-D", s.data, "-m", "immediate", "-w", "stop").Run()
}

// url is a connection to db with the password in it.
func (s *pgServer) url(db string) string {
	return "postgresql://agent:pa%3Ass%40w0rd%20s3cret@127.0.0.1:" + strconv.Itoa(s.port) + "/" + db
}

// psql runs sql in db and returns its unaligned output.
func (s *pgServer) psql(t *testing.T, db, sql string) string {
	t.Helper()
	return s.e.must(s.data, filepath.Join(s.bin, "psql"), "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-d", db, "-c", sql)
}

// saltWithPg is e with Postgres's programs on PATH, and the server's
// variables, password included, left out.
func saltWithPg(e *env, s *pgServer) *env {
	return e.with("PATH=" + filepath.Dir(e.bin) + ":" + s.bin + ":/usr/bin:/bin")
}

// memorySQL fills a database the way an agent might: a table with text,
// JSON and a sequence, and a function.
const memorySQL = `
CREATE TABLE memories (id serial PRIMARY KEY, peer text NOT NULL, body text, meta jsonb);
INSERT INTO memories (peer, body, meta)
	SELECT 'peer' || (i % 3), 'remembered thing ' || i || ' with ''quotes'', tabs	and ✓', jsonb_build_object('n', i)
	FROM generate_series(1, 300) AS i;
CREATE FUNCTION memory_count() RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM memories';
`

// pgSnapshot is what a test compares between the live and restored
// databases.
const pgSnapshot = "SELECT memory_count(), md5(string_agg(id || peer || body || meta::text, ',' ORDER BY id)), (SELECT last_value FROM memories_id_seq) FROM memories"

// salt seal --postgres-env dumps a live database, without its password ever
// on a command line, seals it as memory.sql, and leaves nothing behind. The
// documented restore steps bring back the same database. An unchanged
// database makes no change to the repo; a changed one does.
func TestSealPostgres(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE memory")
	s.psql(t, "memory", memorySQL)
	// pgvector is used when installed.
	_, code := s.e.run(s.data, filepath.Join(s.bin, "psql"), "-X", "-q", "-d", "memory", "-c", "CREATE EXTENSION vector")
	vector := code == 0
	if vector {
		s.psql(t, "memory", "CREATE TABLE embeddings (id int PRIMARY KEY, v vector(3)); INSERT INTO embeddings VALUES (1, '[1,2,3]'), (2, '[0.5,0,-1]');")
	}
	live := s.psql(t, "memory", pgSnapshot)
	b := newBackupRepo(t, e)
	write(t, filepath.Join(b.src, "SOUL.md"), "be kind\n")

	salt, tmp := withTemp(t, saltWithPg(e, s))
	salt = salt.with("AGENT_DB_URL=postgresql+psycopg://agent:pa%3Ass%40w0rd%20s3cret@127.0.0.1:" + strconv.Itoa(s.port) + "/memory")
	out := salt.must(b.base, "salt", "seal", "--postgres-env", "AGENT_DB_URL", b.src, b.dir)
	if strings.Contains(out, "s3cret") || !strings.Contains(out, "sealed 2 files") {
		t.Fatalf("seal output:\n%s", out)
	}
	assertEmpty(t, tmp)
	e.must(b.dir, "git", "add", "-A")
	e.must(b.dir, "git", "commit", "-q", "-m", "backup")
	if grep, _ := e.run(b.dir, "git", "grep", "-l", "-a", "-e", "PostgreSQL database dump", "-e", "remembered thing", "HEAD"); strings.TrimSpace(grep) != "" {
		t.Fatalf("plaintext dump found in commit: %s", grep)
	}

	// The restore steps from the design doc.
	dest := filepath.Join(t.TempDir(), "restored")
	e.must(b.base, "salt", "restore", b.dir, "--to", dest, "memory.sql")
	dump := filepath.Join(dest, "memory.sql")
	if fi, err := os.Stat(dump); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("restored dump: %v, %v", fi, err)
	}
	s.psql(t, "postgres", "CREATE DATABASE memory_restored")
	s.e.must(dest, filepath.Join(s.bin, "psql"), "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-d", "memory_restored", "-f", dump)
	if got := s.psql(t, "memory_restored", pgSnapshot); got != live {
		t.Fatalf("restored database = %q, want %q", got, live)
	}
	if vector {
		if got := s.psql(t, "memory_restored", "SELECT v FROM embeddings ORDER BY id"); got != "[1,2,3]\n[0.5,0,-1]\n" {
			t.Fatalf("restored vectors = %q", got)
		}
	}
	// A pg_dump with --restrict-key is given salt's key for the repo, in
	// place of a random one.
	help := e.must(b.base, filepath.Join(s.bin, "pg_dump"), "--help")
	if data, _ := os.ReadFile(dump); strings.Contains(help, "--restrict-key") && !regexp.MustCompile(`(?m)^\\restrict [0-9a-f]{32}$`).Match(data) {
		t.Fatalf("the dump does not use salt's key:\n%.300s", data)
	}

	// An unchanged database makes no change to the repo, even with a
	// pg_dump that writes a random key into every dump.
	salt.must(b.base, "salt", "seal", "--postgres-env", "AGENT_DB_URL", b.src, b.dir)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an unchanged database changed the repo:\n%s", st)
	}
	s.psql(t, "memory", "INSERT INTO memories (peer, body) VALUES ('peer0', 'something new')")
	if out := salt.must(b.base, "salt", "seal", "--postgres-env", "AGENT_DB_URL", b.src, b.dir); !strings.Contains(out, "1 encrypted, 1 unchanged") {
		t.Fatalf("a changed database: %s", out)
	}
	assertEmpty(t, tmp)
}

// Two servers whose memory is in Postgres's default database, postgres, are
// both backed up and restored when one is given another name with --name,
// which works the same with --postgres-env.
func TestSealPostgresNamed(t *testing.T) {
	e := newEnv(t)
	first, second := startPostgres(t, e), startPostgres(t, e)
	first.psql(t, "postgres", memorySQL)
	second.psql(t, "postgres", memorySQL+"INSERT INTO memories (peer, body) VALUES ('peer9', 'only on the second server');")
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)

	salt, tmp := withTemp(t, saltWithPg(e, first))
	salt = salt.with("HONCHO_DB=" + second.url("postgres"))
	out := salt.must(b.base, "salt", "seal", "--postgres", first.url("postgres"), "--name", "honcho/postgres.sql", "--postgres-env", "HONCHO_DB", b.src, b.dir)
	if strings.Contains(out, "s3cret") {
		t.Fatalf("seal output shows the password:\n%s", out)
	}
	assertEmpty(t, tmp)
	e.must(b.dir, "git", "add", "-A")
	e.must(b.dir, "git", "commit", "-q", "-m", "backup")
	dest := filepath.Join(t.TempDir(), "restored")
	e.must(b.base, "salt", "restore", b.dir, "--to", dest)
	for rel, onSecond := range map[string]bool{"postgres.sql": false, "honcho/postgres.sql": true} {
		data, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil || !strings.Contains(string(data), "remembered thing 300") || strings.Contains(string(data), "only on the second server") != onSecond {
			t.Fatalf("restored %s: %v\n%.300s", rel, err, data)
		}
	}

	// Without --name the two are refused before pg_dump runs, without
	// showing a password.
	out = assertSealFails(t, e.with("PATH="+filepath.Dir(e.bin), "HONCHO_DB="+second.url("postgres")), b,
		"would both be backed up as postgres.sql. Give one of them another name with --name NAME before its --postgres-env",
		"--postgres", first.url("postgres"), "--postgres-env", "HONCHO_DB")
	if strings.Contains(out, "s3cret") || strings.Contains(out, "pa:ss") {
		t.Fatalf("the output shows the password:\n%s", out)
	}
}

// Every way of giving the password works: in the URL given to --postgres,
// in PGPASSWORD, and in ~/.pgpass. libpq settings work like a URL. The
// keepalives salt adds work beside a connection's own.
func TestSealPostgresPasswords(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE notes")
	s.psql(t, "notes", memorySQL)
	port := strconv.Itoa(s.port)
	pgpass := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(pgpass, []byte("127.0.0.1:"+port+":*:agent:"+strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(pgPassword)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noPassword := "postgresql://agent@127.0.0.1:" + port + "/notes"
	for name, tc := range map[string]struct {
		conn string
		vars []string
	}{
		"url":        {s.url("notes"), nil},
		"pgpassword": {noPassword, []string{"PGPASSWORD=" + pgPassword}},
		"pgpass":     {noPassword, []string{"PGPASSFILE=" + pgpass}},
		"settings":   {"host=127.0.0.1 port=" + port + " user=agent dbname=notes password='" + strings.ReplaceAll(pgPassword, "'", `\'`) + "'", nil},
		"keepalives": {s.url("notes") + "?keepalives_idle=300&tcp_user_timeout=0", nil},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBackupRepo(t, e)
			os.MkdirAll(b.src, 0o755)
			salt, tmp := withTemp(t, saltWithPg(e, s))
			salt.with(tc.vars...).must(b.base, "salt", "seal", "--postgres", tc.conn, b.src, b.dir)
			assertEmpty(t, tmp)
			dest := filepath.Join(t.TempDir(), "restored")
			e.must(b.base, "salt", "restore", b.dir, "--to", dest)
			if b, err := os.ReadFile(filepath.Join(dest, "notes.sql")); err != nil || !strings.Contains(string(b), "remembered thing 300") {
				t.Fatalf("restored dump: %v", err)
			}
		})
	}
}

// pg_dump is given keepalives, so a connection that dies part way through a
// dump fails it. A connection's own settings are kept, keepalives turned off
// stay off, and an older libpq is not given tcp_user_timeout, which it would
// refuse.
func TestSealPostgresKeepalives(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	conn := filepath.Join(t.TempDir(), "conn")
	stub := e.stubPath(t, "pg_dump", `case "$1" in --help) exit 0;; --version) echo "pg_dump (PostgreSQL) $SALT_TEST_VERSION"; exit 0;; esac
for a; do case "$a" in --dbname=*) printf '%s' "${a#--dbname=}" > "$SALT_TEST_CONN";; --file=*) echo dump > "${a#--file=}";; esac; done`)
	for _, tc := range []struct{ version, conn, want string }{
		{"17.2", "postgresql://agent:s3cret@127.0.0.1/memory", "postgresql://agent@127.0.0.1/memory?keepalives_idle=30&keepalives_interval=10&keepalives_count=3&tcp_user_timeout=60000"},
		{"11.22", "postgresql://agent:s3cret@127.0.0.1/memory", "postgresql://agent@127.0.0.1/memory?keepalives_idle=30&keepalives_interval=10&keepalives_count=3"},
		{"17.2", "host=127.0.0.1 dbname=memory keepalives_count=9", "host='127.0.0.1' dbname='memory' keepalives_count='9' keepalives_idle=30 keepalives_interval=10 tcp_user_timeout=60000"},
		{"17.2", "service=agent dbname=memory", "service='agent' dbname='memory'"},
		{"17.2", "postgresql://127.0.0.1/memory?keepalives=0", "postgresql://127.0.0.1/memory?keepalives=0"},
		{"17.2", "postgresql://127.0.0.1/memory?keepalives=1", "postgresql://127.0.0.1/memory?keepalives=1&keepalives_idle=30&keepalives_interval=10&keepalives_count=3&tcp_user_timeout=60000"},
	} {
		salt, tmp := withTemp(t, e.with(stub, "SALT_TEST_VERSION="+tc.version, "SALT_TEST_CONN="+conn))
		salt.must(b.base, "salt", "seal", "--postgres", tc.conn, b.src, b.dir)
		assertEmpty(t, tmp)
		if got, err := os.ReadFile(conn); err != nil || string(got) != tc.want {
			t.Errorf("pg_dump %s, %q: given %q, %v; want %q", tc.version, tc.conn, got, err, tc.want)
		}
	}
}

// When the dump cannot be made, salt stops before sealing, says why without
// showing the password, and leaves nothing behind.
func TestSealPostgresFailures(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE memory")
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	port := strconv.Itoa(s.port)
	wrong := "postgresql://agent:wrong-s3cret@127.0.0.1:" + port + "/memory"
	failing := e.stubPath(t, "pg_dump", "case \"$1\" in --help|--version) exit 0;; esac\necho \"pg_dump: error: connection to server lost\" >&2\nexit 1")
	for name, tc := range map[string]struct {
		args []string
		vars []string
		want string
	}{
		"wrong password":   {[]string{"--postgres", wrong}, nil, "copying the database postgresql://agent@127.0.0.1:" + port + "/memory: pg_dump: exit status 1:"},
		"missing database": {[]string{"--postgres", s.url("gone")}, nil, `database "gone" does not exist`},
		"no password":      {[]string{"--postgres", "postgresql://agent@127.0.0.1:" + port + "/memory"}, nil, "no password supplied"},
		"no database name": {[]string{"--postgres", "postgresql://agent:s3cret@127.0.0.1:" + port}, nil, "the connection given to --postgres names no database"},
		"not postgres":     {[]string{"--postgres", "mysql://agent:s3cret@127.0.0.1/memory"}, nil, "is not a Postgres connection"},
		"unset variable":   {[]string{"--postgres-env", "SALT_TEST_UNSET"}, nil, "SALT_TEST_UNSET given to --postgres-env is not set"},
		"no pg_dump":       {[]string{"--postgres", s.url("memory")}, []string{"PATH=" + filepath.Dir(e.bin)}, "salt needs the pg_dump program"},
		"pg_dump fails":    {[]string{"--postgres", s.url("memory")}, []string{failing}, "connection to server lost"},
	} {
		t.Run(name, func(t *testing.T) {
			out := assertSealFails(t, saltWithPg(e, s).with(tc.vars...), b, tc.want, tc.args...)
			if strings.Contains(out, "s3cret") || strings.Contains(out, "pa:ss") {
				t.Fatalf("the output shows the password:\n%s", out)
			}
		})
	}

	// A server that has gone away fails the same way.
	s.stop()
	assertSealFails(t, saltWithPg(e, s), b, "copying the database postgresql://agent@127.0.0.1:"+port+"/memory: pg_dump", "--postgres", s.url("memory"))
}

// Ctrl-C while pg_dump runs stops it, removes the dump and seals nothing.
func TestSealPostgresInterrupted(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	script := `case "$1" in --help|--version) exit 0;; esac
for a; do case "$a" in --file=*) echo partial > "${a#--file=}";; esac; done
touch "$SALT_TEST_STARTED"`
	assertInterruptStopsCopy(t, e, b, "pg_dump", script, "--postgres", "postgresql://agent@127.0.0.1/memory")
}
