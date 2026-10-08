package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode"

	"github.com/spicy-lemonade/salt/internal/proc"
)

// maxHelp caps how much of pg_dump --help is read.
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

// Postgres is a live Postgres database, copied with pg_dump.
type Postgres struct {
	flag string
	// conn is the connection without its password. pg_dump gets the
	// password from PGPASSWORD, so it never shows in a process list.
	conn, password, dbname string
}

// NewPostgres returns the database a Postgres connection names. conn is a URL
// (postgresql://user:password@host:5432/dbname?settings) or libpq settings
// ("host=localhost dbname=memory"). A driver in the URL's scheme, such as
// postgresql+psycopg://, is dropped, since pg_dump has its own. Without a
// password in conn, pg_dump looks in ~/.pgpass, PGPASSFILE and PGPASSWORD.
func NewPostgres(conn string) (Database, error) {
	return newPostgres("--postgres", "the connection given to --postgres", conn)
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
	conn, password, dbname, err := parse(s)
	if dbname == "" {
		// As in libpq, which pg_dump uses.
		dbname = os.Getenv("PGDATABASE")
	}
	switch {
	case err != nil:
		// parse's error says what is wrong.
	case dbname == "":
		err = errNoDatabase
	case dbname == "." || dbname == ".." || strings.ContainsAny(dbname, "/\\") || strings.ContainsFunc(dbname, unicode.IsControl):
		err = errBadName
	}
	if err != nil {
		return nil, fmt.Errorf("%s %w", where, err)
	}
	return &Postgres{flag: flag, conn: conn, password: password, dbname: dbname}, nil
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
	// pg_dump --help says whether it has --restrict-key.
	help := &proc.LimitedBuffer{Max: maxHelp}
	cmd := exec.CommandContext(ctx, "pg_dump", "--help")
	cmd.Stdout = help
	if err := proc.Run(ctx, cmd); err != nil {
		return Meta{}, err
	}
	if err := proc.Run(ctx, pgDumpCommand(ctx, p, o, help.String())); err != nil {
		return Meta{}, err
	}
	return Meta{Mode: 0o600}, nil
}

// pgDumpCommand builds the pg_dump command that dumps p into o.Dst. It never
// lets pg_dump ask for a password, which would stop a scheduled backup until
// someone answers, and gives up on a table another program has locked after
// waitTimeout instead of waiting forever. When help lists --restrict-key,
// o.Key replaces the random key that pg_dump would write into every dump.
//
// The password goes in PGPASSWORD, where it wins over any already set, as it
// would in the connection. Unless the person set their own
// PGCONNECT_TIMEOUT, each connection attempt is limited to waitTimeout, so
// an unreachable server fails the backup instead of stopping it.
func pgDumpCommand(ctx context.Context, p *Postgres, o CopyOptions, help string) *exec.Cmd {
	args := []string{
		"--no-password",
		"--format=plain",
		"--lock-wait-timeout=" + strconv.FormatInt(waitTimeout.Milliseconds(), 10),
		"--file=" + o.Dst,
	}
	if strings.Contains(help, "--restrict-key") {
		args = append(args, "--restrict-key="+o.Key)
	}
	cmd := exec.CommandContext(ctx, "pg_dump", append(args, "--dbname="+p.conn)...)
	cmd.Env = os.Environ()
	if p.password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+p.password)
	}
	if _, set := os.LookupEnv("PGCONNECT_TIMEOUT"); !set {
		cmd.Env = append(cmd.Env, "PGCONNECT_TIMEOUT="+strconv.Itoa(int(waitTimeout.Seconds())))
	}
	return cmd
}

// parseURL reads a URL in libpq's form. Everything but the password is kept
// as written, so pg_dump reads the same connection.
func parseURL(s string) (conn, password, dbname string, err error) {
	scheme, rest, _ := strings.Cut(s, "://")
	if base, _, _ := strings.Cut(scheme, "+"); base != "postgresql" && base != "postgres" {
		return "", "", "", errNotPostgres
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		if user, pw, ok := strings.Cut(authority[:at], ":"); ok {
			if password, err = url.PathUnescape(pw); err != nil {
				return "", "", "", errNotPostgres
			}
			authority = user + authority[at:]
		}
	}
	path, query, _ := strings.Cut(tail, "?")
	if dbname, err = url.PathUnescape(strings.TrimPrefix(path, "/")); err != nil {
		return "", "", "", errNotPostgres
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
			return "", "", "", errNotPostgres
		case key == "password":
			password = value
			continue
		case key == "dbname":
			dbname = value
		}
		kept = append(kept, kv)
	}
	conn = "postgresql://" + authority + path
	if len(kept) > 0 {
		conn += "?" + strings.Join(kept, "&")
	}
	return conn, password, dbname, nil
}

// parseSettings reads libpq's key=value settings, where a value may be
// single-quoted and a backslash escapes the next character.
func parseSettings(s string) (conn, password, dbname string, err error) {
	var kept []string
	rest := strings.TrimLeft(s, spaces)
	if rest == "" {
		return "", "", "", errNotPostgres
	}
	for rest != "" {
		key, after, ok := strings.Cut(rest, "=")
		key = strings.TrimRight(key, spaces)
		if !ok || key == "" || strings.ContainsAny(key, spaces) {
			return "", "", "", errNotPostgres
		}
		var value string
		if value, rest, ok = settingValue(strings.TrimLeft(after, spaces)); !ok {
			return "", "", "", errNotPostgres
		}
		rest = strings.TrimLeft(rest, spaces)
		switch key {
		case "password":
			password = value
			continue
		case "dbname":
			dbname = value
		}
		kept = append(kept, key+"='"+settingQuoter.Replace(value)+"'")
	}
	return strings.Join(kept, " "), password, dbname, nil
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
