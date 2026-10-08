package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// maxHelp caps how much of pg_dump --help or --version is read.
const maxHelp = 64 << 10

// spaces separate the settings in a keyword connection string.
const spaces = " \t\n\r\f\v"

// settingQuoter escapes a setting's value for single quotes, so libpq reads
// it back unchanged.
var settingQuoter = strings.NewReplacer(`\`, `\\`, `'`, `\'`)

var (
	errNotPostgres = errors.New("is not a Postgres connection; use a URL such as postgresql://user@host:5432/dbname or settings such as \"host=localhost dbname=memory\"")
	errNoDatabase  = errors.New("names no database; add its name, as in postgresql://host/dbname or dbname=name, or set PGDATABASE")
	errBadName     = errors.New("names a database whose name cannot be used as a file name")
)

// keepalives are the libpq settings that make pg_dump notice a connection
// that dies part way through a dump, such as when the server's container
// restarts or the machine sleeps, after about a minute without a reply.
// Without them, pg_dump waits on it until the system gives up, about two
// hours. A server that is busy but reachable still answers, so a long dump is
// never stopped. since is the first libpq version that knows the setting.
var keepalives = []struct {
	key, value string
	since      int
}{
	{"keepalives_idle", "30", 0},
	{"keepalives_interval", "10", 0},
	{"keepalives_count", "3", 0},
	// For a connection that dies while pg_dump is sending, which keepalives
	// miss. Only Linux uses it.
	{"tcp_user_timeout", "60000", 12},
}

// Postgres is a live Postgres database, copied with pg_dump.
type Postgres struct {
	flag string
	// conn is the connection without its password. pg_dump gets the
	// password from PGPASSWORD, so it never shows in a process list.
	conn, password, dbname string
	// keys names the settings conn gives. isURL is whether conn is a URL
	// rather than settings, and hasQuery whether that URL has settings.
	keys            []string
	isURL, hasQuery bool
	// keepalives is the value conn gives libpq's keepalives setting, if any.
	keepalives string
}

// pgDump is what salt needs to know about the pg_dump it runs.
type pgDump struct {
	// restrictKey is whether pg_dump has --restrict-key.
	restrictKey bool
	// libpq is the major version of pg_dump's libpq, or 0 if unknown.
	libpq int
}

// newPgDump reads pg_dump's --help and --version output. --version reads as
// "pg_dump (PostgreSQL) 17.2", and pg_dump's libpq is at least as new.
func newPgDump(help, version string) pgDump {
	d := pgDump{restrictKey: strings.Contains(help, "--restrict-key")}
	if _, v, ok := strings.Cut(version, ") "); ok {
		fmt.Sscanf(v, "%d", &d.libpq)
	}
	return d
}

// NewPostgres returns the database a Postgres connection names. conn is a URL
// (postgresql://user:password@host:5432/dbname?settings) or libpq settings
// ("host=localhost dbname=memory"). A driver in the URL's scheme, such as
// postgresql+psycopg://, is dropped, since pg_dump has its own. Without a
// password in conn, pg_dump looks in ~/.pgpass, PGPASSFILE and PGPASSWORD.
func NewPostgres(conn string) (Database, error) {
	return NewPostgresConn(conn, "the connection given to --postgres")
}

// NewPostgresEnv is NewPostgres for the connection held by the environment
// variable name, so that its password is never typed on a command line.
func NewPostgresEnv(name string) (Database, error) {
	conn := os.Getenv(name)
	if conn == "" {
		return nil, fmt.Errorf("the environment variable %s given to --postgres-env is not set or is empty", name)
	}
	return newPostgres("--postgres-env", fmt.Sprintf("the connection in %s, given to --postgres-env,", name), conn)
}

// NewPostgresConn is NewPostgres for a connection salt was given another
// way, such as by a preset. where names the connection in messages.
func NewPostgresConn(conn, where string) (Database, error) {
	return newPostgres("--postgres", where, conn)
}

// newPostgres reads the connection s, given with the option flag. Its errors
// start with where, which names the connection, and never repeat a password.
func newPostgres(flag, where, s string) (Database, error) {
	parse := parseSettings
	if scheme, _, ok := strings.Cut(s, "://"); ok && !strings.ContainsAny(scheme, spaces+"=") {
		parse = parseURL
	}
	p, err := parse(s)
	if p.dbname == "" {
		// As in libpq, which pg_dump uses.
		p.dbname = os.Getenv("PGDATABASE")
	}
	switch {
	case err != nil:
		// parse's error says what is wrong.
	case p.dbname == "":
		err = errNoDatabase
	case p.dbname == "." || p.dbname == ".." || strings.ContainsAny(p.dbname, "/\\") || strings.ContainsFunc(p.dbname, unicode.IsControl):
		err = errBadName
	}
	if err != nil {
		return nil, fmt.Errorf("%s %w", where, err)
	}
	p.flag = flag
	return &p, nil
}

// Name is the database's name with .sql, the extension of pg_dump's plain
// SQL output.
func (p *Postgres) Name() string   { return p.dbname + ".sql" }
func (p *Postgres) String() string { return p.conn }
func (p *Postgres) Flag() string   { return p.flag }

// Copy dumps the database as plain SQL with pg_dump, which reads it in one
// transaction, so the dump is consistent while other programs write. pg_dump
// streams it to o.Dst, so it is never held in memory. The dump is owner-only.
// It has no last-modified date of its own, so none is recorded, which also
// keeps an unchanged database from making a commit. A restored dump is dated
// when it is restored.
func (p *Postgres) Copy(ctx context.Context, o CopyOptions) (Meta, error) {
	// pg_dump --help says whether it has --restrict-key, and --version which
	// keepalives its libpq knows.
	var out [2]proc.LimitedBuffer
	for i, arg := range []string{"--help", "--version"} {
		out[i].Max = maxHelp
		cmd := exec.CommandContext(ctx, "pg_dump", arg)
		cmd.Stdout = &out[i]
		if err := proc.Run(ctx, cmd); err != nil {
			return Meta{}, err
		}
	}
	if err := proc.Run(ctx, pgDumpCommand(ctx, p, o, newPgDump(out[0].String(), out[1].String()))); err != nil {
		return Meta{}, err
	}
	return Meta{Mode: 0o600}, nil
}

// pgDumpCommand builds the pg_dump command that dumps p into o.Dst. It never
// lets pg_dump ask for a password, which would stop a scheduled backup until
// someone answers, and gives up on a table another program has locked after
// waitTimeout instead of waiting forever. When d has --restrict-key, o.Key
// replaces the random key that pg_dump would write into every dump.
//
// The password goes in PGPASSWORD, where it wins over any already set, as it
// would in the connection. Unless the person set their own
// PGCONNECT_TIMEOUT, each connection attempt is limited to waitTimeout, so
// an unreachable server fails the backup instead of stopping it. The
// connection gets keepalives (see dumpConn), so one that dies part way
// through a dump fails it too.
func pgDumpCommand(ctx context.Context, p *Postgres, o CopyOptions, d pgDump) *exec.Cmd {
	args := []string{
		"--no-password",
		"--format=plain",
		"--lock-wait-timeout=" + strconv.FormatInt(waitTimeout.Milliseconds(), 10),
		"--file=" + o.Dst,
	}
	if d.restrictKey {
		args = append(args, "--restrict-key="+o.Key)
	}
	cmd := exec.CommandContext(ctx, "pg_dump", append(args, "--dbname="+p.dumpConn(d.libpq))...)
	cmd.Env = os.Environ()
	if p.password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+p.password)
	}
	if _, set := os.LookupEnv("PGCONNECT_TIMEOUT"); !set {
		cmd.Env = append(cmd.Env, "PGCONNECT_TIMEOUT="+strconv.Itoa(int(waitTimeout.Seconds())))
	}
	return cmd
}

// dumpConn is p's connection with each of keepalives it does not set itself
// added, leaving out those libpq, pg_dump's libpq version, is too old for.
// A connection that turns keepalives off, with keepalives=0, gets none.
// Nor does one that names a libpq service, or runs with PGSERVICE set, as
// the service file may set its own and the connection's settings would win
// over them.
func (p *Postgres) dumpConn(libpq int) string {
	// libpq reads keepalives as a whole number, and only 0 turns them off.
	off, err := strconv.Atoi(strings.TrimSpace(p.keepalives))
	if _, set := os.LookupEnv("PGSERVICE"); set || slices.Contains(p.keys, "service") || (err == nil && off == 0) {
		return p.conn
	}
	var add []string
	for _, k := range keepalives {
		if libpq >= k.since && !slices.Contains(p.keys, k.key) {
			add = append(add, k.key+"="+k.value)
		}
	}
	switch {
	case len(add) == 0:
		return p.conn
	case !p.isURL:
		return p.conn + " " + strings.Join(add, " ")
	case p.hasQuery:
		return p.conn + "&" + strings.Join(add, "&")
	default:
		return p.conn + "?" + strings.Join(add, "&")
	}
}

// parseURL reads a URL in libpq's form. Everything but the password is kept
// as written, so pg_dump reads the same connection.
//
// As in libpq, the user and password end at the first "@" before any "/",
// so a password may hold "?". A later "@" before the "/", with no "?" in
// between, ends them instead, so a password may hold "@" too. A host never
// holds one, so libpq could not read such a URL anyway.
func parseURL(s string) (p Postgres, err error) {
	scheme, rest, _ := strings.Cut(s, "://")
	if base, _, _ := strings.Cut(scheme, "+"); base != "postgresql" && base != "postgres" {
		return Postgres{}, errNotPostgres
	}
	before, _, _ := strings.Cut(rest, "/")
	at := strings.IndexByte(before, '@')
	for at >= 0 {
		next := strings.IndexAny(before[at+1:], "@?")
		if next < 0 || before[at+1+next] != '@' {
			break
		}
		at += 1 + next
	}
	var user string
	if at >= 0 {
		u, pw, ok := strings.Cut(rest[:at], ":")
		if ok {
			if p.password, err = url.PathUnescape(pw); err != nil {
				return Postgres{}, errNotPostgres
			}
		}
		user, rest = u+"@", rest[at+1:]
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	authority = user + authority
	path, query, _ := strings.Cut(tail, "?")
	if p.dbname, err = url.PathUnescape(strings.TrimPrefix(path, "/")); err != nil {
		return Postgres{}, errNotPostgres
	}
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		switch {
		case kv == "":
			continue
		case err1 != nil || err2 != nil:
			return Postgres{}, errNotPostgres
		case key == "password":
			p.password = value
			continue
		case key == "dbname":
			p.dbname = value
		case key == "keepalives":
			p.keepalives = value
		}
		kept = append(kept, kv)
		p.keys = append(p.keys, key)
	}
	p.conn = "postgresql://" + authority + path
	p.hasQuery = len(kept) > 0
	if p.hasQuery {
		p.conn += "?" + strings.Join(kept, "&")
	}
	p.isURL = true
	return p, nil
}

// parseSettings reads libpq's key=value settings, where a value may be
// single-quoted and a backslash escapes the next character.
func parseSettings(s string) (p Postgres, err error) {
	var kept []string
	rest := strings.TrimLeft(s, spaces)
	if rest == "" {
		return Postgres{}, errNotPostgres
	}
	for rest != "" {
		key, after, ok := strings.Cut(rest, "=")
		key = strings.TrimRight(key, spaces)
		if !ok || key == "" || strings.ContainsAny(key, spaces) {
			return Postgres{}, errNotPostgres
		}
		var value string
		if value, rest, ok = settingValue(strings.TrimLeft(after, spaces)); !ok {
			return Postgres{}, errNotPostgres
		}
		rest = strings.TrimLeft(rest, spaces)
		switch key {
		case "password":
			p.password = value
			continue
		case "dbname":
			p.dbname = value
		case "keepalives":
			p.keepalives = value
		}
		kept = append(kept, key+"='"+settingQuoter.Replace(value)+"'")
		p.keys = append(p.keys, key)
	}
	p.conn = strings.Join(kept, " ")
	return p, nil
}

// settingValue reads one value from the start of s and returns the rest.
func settingValue(s string) (value, rest string, ok bool) {
	quoted := strings.HasPrefix(s, "'")
	if quoted {
		s = s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case quoted && c == '\'':
			return b.String(), s[i+1:], true
		case !quoted && strings.IndexByte(spaces, c) >= 0:
			return b.String(), s[i:], true
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), "", !quoted
}
