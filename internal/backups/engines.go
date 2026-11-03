package backups

import (
	"errors"
	"path/filepath"
	"strconv"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// The engines' dumps and restores are short sh scripts run where the
// database is (its container, or as the site's processes run). What a
// backup says (database, user, host, the name of the password's
// variable) reaches them as MW_* variables, never as script text: a
// database called "x; rm -rf ." is a database's name to the dump and
// nothing else. The password is read from the variable it names there,
// where it already is, and handed to the client through its own variable
// (MYSQL_PWD, PGPASSWORD), never as an argument other users could read
// in ps. mongodump and mongorestore have no such variable, and take it as
// an argument.

// prelude is every engine's start: stop at the first error, the
// password from the variable the backup names, and pick, the first of
// a list of programs that is installed (mariadb-dump replaced mysqldump,
// and an image has one or the other).
const prelude = `set -e
pw=""
if [ -n "$MW_PASSWORD_ENV" ]; then eval "pw=\${$MW_PASSWORD_ENV:-}"; fi
pick() {
  for c in "$@"; do
    if command -v "$c" >/dev/null 2>&1; then echo "$c"; return 0; fi
  done
  echo "none of $* is installed where the database is" >&2
  return 127
}
`

// mysqlCommon finds who connects and how, for the dump and the restore.
// Without a user it is root with the image's root password; with one, the
// image's password for its user, unless password_env says where it is.
const mysqlCommon = `u="${MW_USER:-}"
if [ -z "$u" ]; then
  u=root
  if [ -z "$MW_PASSWORD_ENV" ]; then pw="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; fi
elif [ -z "$MW_PASSWORD_ENV" ]; then
  pw="${MARIADB_PASSWORD:-${MYSQL_PASSWORD:-}}"
fi
if [ -n "$pw" ]; then export MYSQL_PWD="$pw"; fi
db="${MW_DATABASE:-${MARIADB_DATABASE:-${MYSQL_DATABASE:-}}}"
set -- -u "$u"
if [ -n "$MW_HOST" ]; then set -- "$@" -h "$MW_HOST" --protocol=tcp; fi
if [ -n "$MW_PORT" ]; then set -- "$@" -P "$MW_PORT"; fi
`

// One database is dumped with --databases, so the copy says which one it
// is (CREATE DATABASE, USE) and the restore needs no name. Tables are
// dropped before they are made again (the default --add-drop-table).
const mysqlDump = prelude + mysqlCommon + `bin=$(pick mariadb-dump mysqldump)
set -- "$@" --single-transaction --quick --hex-blob --triggers --routines
if [ -z "$db" ] || [ "$db" = "*" ]; then set -- "$@" --all-databases --events; else set -- "$@" --databases "$db"; fi
exec "$bin" "$@"
`

const mysqlRestore = prelude + mysqlCommon + `bin=$(pick mariadb mysql)
exec "$bin" "$@"
`

// pgCommon: the image's POSTGRES_USER (postgres when unset) through the
// container's local socket, which the official image trusts.
const pgCommon = `u="${MW_USER:-${POSTGRES_USER:-postgres}}"
if [ -z "$MW_PASSWORD_ENV" ]; then pw="${POSTGRES_PASSWORD:-}"; fi
if [ -n "$pw" ]; then export PGPASSWORD="$pw"; fi
db="${MW_DATABASE:-${POSTGRES_DB:-}}"
set -- -U "$u" --no-password
if [ -n "$MW_HOST" ]; then set -- "$@" -h "$MW_HOST"; fi
if [ -n "$MW_PORT" ]; then set -- "$@" -p "$MW_PORT"; fi
`

// --clean --if-exists: the copy drops what it makes before making it, so
// a restore over the database as it is now ends with the copy's, not
// with both.
const pgDump = prelude + pgCommon + `if [ -z "$db" ] || [ "$db" = "*" ]; then
  bin=$(pick pg_dumpall)
  exec "$bin" "$@" --clean --if-exists
fi
bin=$(pick pg_dump)
exec "$bin" "$@" --clean --if-exists "$db"
`

// One database is restored in one transaction, stopping at the first
// error: half a restore is worse than none. A whole cluster cannot be
// (CREATE DATABASE runs outside one), nor stop at its first error (it
// drops roles, the one connected among them).
const pgRestore = prelude + pgCommon + `bin=$(pick psql)
if [ -z "$db" ] || [ "$db" = "*" ]; then exec "$bin" "$@" -X -q -d postgres; fi
exec "$bin" "$@" -X -q -v ON_ERROR_STOP=1 --single-transaction -d "$db"
`

const mongoCommon = `u="${MW_USER:-${MONGO_INITDB_ROOT_USERNAME:-}}"
if [ -z "$MW_PASSWORD_ENV" ]; then pw="${MONGO_INITDB_ROOT_PASSWORD:-}"; fi
db="${MW_DATABASE:-${MONGO_INITDB_DATABASE:-}}"
set -- --archive --quiet
if [ -n "$MW_HOST" ]; then set -- "$@" --host "$MW_HOST"; fi
if [ -n "$MW_PORT" ]; then set -- "$@" --port "$MW_PORT"; fi
if [ -n "$u" ]; then
  authdb=admin
  if [ -n "$MW_USER" ] && [ -n "$db" ] && [ "$db" != "*" ]; then authdb="$db"; fi
  set -- "$@" --username "$u" --password "$pw" --authenticationDatabase "$authdb"
fi
`

const mongoDump = prelude + mongoCommon + `bin=$(pick mongodump)
if [ -n "$db" ] && [ "$db" != "*" ]; then set -- "$@" --db "$db"; fi
exec "$bin" "$@"
`

const mongoRestore = prelude + mongoCommon + `bin=$(pick mongorestore)
if [ -n "$db" ] && [ "$db" != "*" ]; then set -- "$@" --nsInclude "$db.*"; fi
exec "$bin" "$@" --drop
`

// The file is checked first: sqlite3 given a name that is not there makes
// an empty database by that name, and the copy of nothing would look like
// a copy. .dump reads in one transaction, so a site writing meanwhile
// does not tear it.
const sqliteDump = prelude + `if [ ! -f "$MW_FILE" ]; then echo "$MW_FILE: no such file" >&2; exit 1; fi
bin=$(pick sqlite3)
exec "$bin" -bail "$MW_FILE" .dump
`

// The copy is loaded into a new file beside the database and put in its
// place only once it loaded whole; the journal of the old one goes with
// it, or SQLite would replay it over the new file.
const sqliteRestore = prelude + `t="$MW_FILE.restoring"
trap 'rm -f "$t"' EXIT
bin=$(pick sqlite3)
rm -f "$t"
"$bin" -bail "$t"
rm -f "$MW_FILE-wal" "$MW_FILE-shm" "$MW_FILE-journal"
mv -f "$t" "$MW_FILE"
`

// ErrNoRestore is a command backup without a restore command.
var ErrNoRestore = errors.New("this backup has no restore command: download the copy and restore it by hand, or write backups.<name>.restore")

// Ext is the extension a copy of this engine is saved with, before .gz.
func Ext(engine string) string {
	switch engine {
	case "mongodb":
		return "archive"
	case "command":
		return "dump"
	}
	return "sql"
}

// command is what runs for a dump (restore false) or a restore, and the
// variables it is given. dir is the site's folder; local says the command
// runs as the site's processes do, not inside a container, where a client
// finds no socket (the jail has no /run) and is sent to 127.0.0.1.
func command(b *manifest.Backup, restore, local bool, domain, dir string) ([]string, map[string]string, error) {
	env := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	var script string
	switch b.Engine {
	case "mysql":
		script = either(restore, mysqlDump, mysqlRestore)
	case "postgres":
		script = either(restore, pgDump, pgRestore)
	case "mongodb":
		script = either(restore, mongoDump, mongoRestore)
	case "sqlite":
		script = either(restore, sqliteDump, sqliteRestore)
		set("MW_FILE", filepath.Join(dir, filepath.FromSlash(b.File)))
	case "command":
		argv := b.Dump
		if restore {
			argv = b.Restore
		}
		if len(argv) == 0 {
			return nil, nil, ErrNoRestore
		}
		vars := map[string]string{"DOMAIN": domain, "SITE_DIR": dir}
		out := make([]string, len(argv))
		for i, a := range argv {
			out[i] = manifest.Expand(a, vars)
		}
		return out, env, nil
	default:
		return nil, nil, errors.New("engine " + b.Engine + " is not known")
	}
	host := b.Host
	if host == "" && local && b.Engine != "sqlite" {
		host = "127.0.0.1"
	}
	set("MW_HOST", host)
	if b.Port > 0 {
		set("MW_PORT", strconv.Itoa(b.Port))
	}
	set("MW_USER", b.User)
	set("MW_DATABASE", b.Database)
	set("MW_PASSWORD_ENV", b.PasswordEnv)
	return []string{"sh", "-c", script}, env, nil
}

func either(restore bool, dump, rest string) string {
	if restore {
		return rest
	}
	return dump
}
