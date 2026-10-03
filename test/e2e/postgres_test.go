//go:build e2e

package e2e

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// pgBin finds the folder holding Postgres's programs, or skips the test.
func pgBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		return filepath.Dir(p)
	}
	// Debian and Ubuntu keep them out of PATH, one folder per version.
	dirs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	slices.SortFunc(dirs, func(a, b string) int {
		va, _ := strconv.Atoi(filepath.Base(filepath.Dir(a)))
		vb, _ := strconv.Atoi(filepath.Base(filepath.Dir(b)))
		return va - vb
	})
	if len(dirs) > 0 {
		return dirs[len(dirs)-1]
	}
	t.Skip("Postgres is not installed")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startPostgres starts a new server in a temporary folder and stops it when
// the test ends. It has no Unix socket, so every connection uses TCP and the
// password.
func startPostgres(t *testing.T, e *env) *pgServer {
	t.Helper()
	bin := pgBin(t)
	// Without a locale, Postgres on macOS refuses to start.
	e = e.with("LC_ALL=C")
	base := t.TempDir()
	s := &pgServer{bin: bin, data: filepath.Join(base, "data"), port: freePort(t)}
	pw := filepath.Join(base, "pw")
	write(t, pw, pgPassword+"\n")
	e.must(base, filepath.Join(bin, "initdb"), "-D", s.data, "-U", "agent", "--pwfile", pw, "--auth", "scram-sha-256", "-E", "UTF8", "--no-locale")
	opts := "-c listen_addresses=127.0.0.1 -c unix_socket_directories='' -c port=" + strconv.Itoa(s.port)
	if out, code := e.run(base, filepath.Join(bin, "pg_ctl"), "-D", s.data, "-o", opts, "-l", filepath.Join(base, "log"), "-w", "-t", "30", "start"); code != 0 {
		log, _ := os.ReadFile(filepath.Join(base, "log"))
		t.Fatalf("the Postgres server did not start: %s\n%s", out, log)
	}
	t.Cleanup(func() { s.stop() })
	s.e = e.with("PGHOST=127.0.0.1", "PGPORT="+strconv.Itoa(s.port), "PGUSER=agent", "PGPASSWORD="+pgPassword)
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
// JSON and a sequence, and a function. pgvector is used when installed.
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

// hasVector reports whether pgvector can be installed in db, and installs it.
func (s *pgServer) hasVector(t *testing.T, db string) bool {
	t.Helper()
	if _, code := s.e.run(s.data, filepath.Join(s.bin, "psql"), "-X", "-q", "-d", db, "-c", "CREATE EXTENSION vector"); code != 0 {
		return false
	}
	s.psql(t, db, "CREATE TABLE embeddings (id int PRIMARY KEY, v vector(3)); INSERT INTO embeddings VALUES (1, '[1,2,3]'), (2, '[0.5,0,-1]');")
	return true
}

// salt seal --postgres-env dumps a live database, without its password ever
// on a command line, seals it as memory.sql, and leaves nothing behind. The
// documented restore steps bring back the same database. An unchanged
// database makes no change to the repo; a changed one does.
func TestSealPostgres(t *testing.T) {
	e := newEnv(t)
	s := startPostgres(t, e)
	s.psql(t, "postgres", "CREATE DATABASE memory")
	s.psql(t, "memory", memorySQL)
	vector := s.hasVector(t, "memory")
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

	// The restore steps from the README.
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
	// A pg_dump with --restrict-key is given the repo's key, kept in salt's
	// cache, in place of a random one.
	if help := e.must(b.base, filepath.Join(s.bin, "pg_dump"), "--help"); strings.Contains(help, "--restrict-key") {
		keys, _ := filepath.Glob(filepath.Join(e.home, "*", "*", "salt", "copykey-*"))
		if more, _ := filepath.Glob(filepath.Join(e.home, "*", "salt", "copykey-*")); len(more) > 0 {
			keys = append(keys, more...)
		}
		if len(keys) != 1 {
			t.Fatalf("copy keys: %v", keys)
		}
		key, _ := os.ReadFile(keys[0])
		if data, _ := os.ReadFile(dump); !strings.Contains(string(data), `\restrict `+string(key)+"\n") {
			t.Fatalf("the dump does not use the repo's key %s", key)
		}
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

// Every way of giving the password works: in the URL given to --postgres,
// in PGPASSWORD, and in ~/.pgpass. libpq settings work like a URL.
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
	failing := t.TempDir()
	if err := os.WriteFile(filepath.Join(failing, "pg_dump"), []byte("#!/bin/sh\necho \"pg_dump: error: connection to server lost\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
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
		"pg_dump fails":    {[]string{"--postgres", s.url("memory")}, []string{"PATH=" + failing + ":" + filepath.Dir(e.bin) + ":/usr/bin:/bin"}, "connection to server lost"},
	} {
		t.Run(name, func(t *testing.T) {
			salt, tmp := withTemp(t, saltWithPg(e, s))
			out, code := salt.with(tc.vars...).run(b.base, "salt", append(append([]string{"seal"}, tc.args...), b.src, b.dir)...)
			if code != 1 || !strings.HasPrefix(out, "salt: ") || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want 1 and %q:\n%s", code, tc.want, out)
			}
			if strings.Contains(out, "s3cret") || strings.Contains(out, "pa:ss") {
				t.Fatalf("the output shows the password:\n%s", out)
			}
			assertEmpty(t, tmp)
			if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
				t.Fatalf("a failed dump changed the repo:\n%s", st)
			}
		})
	}

	// A server that has gone away fails the same way.
	s.stop()
	salt, tmp := withTemp(t, saltWithPg(e, s))
	out, code := salt.run(b.base, "salt", "seal", "--postgres", s.url("memory"), b.src, b.dir)
	if code != 1 || !strings.Contains(out, "copying the database postgresql://agent@127.0.0.1:"+port+"/memory: pg_dump") {
		t.Fatalf("server stopped: exit %d:\n%s", code, out)
	}
	assertEmpty(t, tmp)
}

// Ctrl-C while pg_dump runs stops it, removes the dump and seals nothing. A
// stand-in pg_dump starts a dump, says so, then waits.
func TestSealPostgresInterrupted(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	os.MkdirAll(b.src, 0o755)
	bin := t.TempDir()
	started := filepath.Join(t.TempDir(), "started")
	fake := "#!/bin/sh\ncase \"$1\" in --help) exit 0;; esac\nfor a; do case \"$a\" in --file=*) echo partial > \"${a#--file=}\";; esac; done\ntouch " + started + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	salt, tmp := withTemp(t, e)
	salt = salt.with("PATH=" + bin + ":" + filepath.Dir(e.bin) + ":/usr/bin:/bin")
	cmd := exec.Command(e.bin, "seal", "--postgres", "postgresql://agent@127.0.0.1/memory", b.src, b.dir)
	cmd.Env = salt.vars
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pg_dump never started")
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 || !strings.Contains(out.String(), "seal interrupted: the database copies were removed and nothing was sealed") {
		t.Fatalf("exit %v, want 130:\n%s", err, out.String())
	}
	assertEmpty(t, tmp)
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("an interrupted seal changed the repo:\n%s", st)
	}
}
