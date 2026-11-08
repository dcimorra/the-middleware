package backups

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// The database browser reads a site's databases where they are, the way
// a backup's dump does (the same target, the same MW_* variables, the
// same way to find the password), and asks the database itself to refuse
// a write: sqlite3 -readonly, a PostgreSQL session whose transactions are
// read-only by default, a MySQL session made READ ONLY. That is a guard
// against a mistake typed into a box, not a wall: whoever may run a query
// here may also run a command where the site runs (exec), and the route
// is in that same permission.

// Database is one database of a site the browser can read.
type Database struct {
	// Name is what a query names it by: a backup's name, or "file:" and
	// the path of a SQLite file found in the folder that no backup names.
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	File    string `json:"file,omitempty"`
	Service string `json:"service,omitempty"`
}

// QueryResult is what a query returned, cut to its first rows.
type QueryResult struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Truncated bool       `json:"truncated,omitempty"`
	Millis    int64      `json:"ms"`
}

// MaxQueryRows and maxQueryBytes bound what one query hands back: a
// SELECT * of a table of millions is a page of the first ones, and a
// panel that froze on the rest would be no browser.
const (
	MaxQueryRows  = 1000
	maxQueryBytes = 4 << 20
	queryTimeout  = 30 * time.Second
	filePrefix    = "file:"
)

// ErrNoDatabase says the name is not a database of the site.
var ErrNoDatabase = errors.New("no such database in this site")

// Databases are the site's databases: the ones its backups name (the
// engines that answer SQL) and the SQLite files in its folder no backup
// names, found by their first bytes rather than their names.
func (k *Keeper) Databases(domain string) ([]Database, error) {
	dir := k.siteDir(domain)
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	var out []Database
	named := map[string]bool{}
	names := make([]string, 0, len(m.Backups))
	for n := range m.Backups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := m.Backups[n]
		if b == nil {
			continue
		}
		switch b.Engine {
		case "sqlite", "postgres", "mysql":
		default:
			continue
		}
		if b.Engine == "sqlite" {
			named[filepath.ToSlash(filepath.Clean(b.File))] = true
		}
		out = append(out, Database{Name: n, Engine: b.Engine, File: b.File, Service: b.Service})
	}
	for _, rel := range sqliteFiles(dir) {
		if !named[rel] {
			out = append(out, Database{Name: filePrefix + rel, Engine: "sqlite", File: rel})
		}
	}
	return out, nil
}

// sqliteFiles are the SQLite databases under dir, a few levels deep, not
// in what a package manager or git keeps.
func sqliteFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel != "." && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "vendor" || strings.Count(rel, string(filepath.Separator)) >= 5) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && isSQLite(p) {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func isSQLite(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return n == 16 && bytes.Equal(head, []byte("SQLite format 3\x00"))
}

// backupFor is the backup a database name stands for.
func (k *Keeper) backupFor(domain, name string) (*manifest.Manifest, *manifest.Backup, error) {
	m, err := manifest.Load(k.siteDir(domain))
	if err != nil {
		return nil, nil, err
	}
	if rel, ok := strings.CutPrefix(name, filePrefix); ok {
		for _, f := range sqliteFiles(k.siteDir(domain)) {
			if f == rel {
				return m, &manifest.Backup{Engine: "sqlite", File: rel}, nil
			}
		}
		return nil, nil, ErrNoDatabase
	}
	b := m.Backups[name]
	if b == nil {
		return nil, nil, ErrNoDatabase
	}
	switch b.Engine {
	case "sqlite", "postgres", "mysql":
		return m, b, nil
	}
	return nil, nil, fmt.Errorf("%s is a %s database: the browser reads SQLite, PostgreSQL and MySQL", name, b.Engine)
}

// Tables lists a database's tables and views.
func (k *Keeper) Tables(ctx context.Context, domain, name string) (QueryResult, error) {
	_, b, err := k.backupFor(domain, name)
	if err != nil {
		return QueryResult{}, err
	}
	var sql string
	switch b.Engine {
	case "sqlite":
		sql = "SELECT name, type FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name"
	case "postgres":
		sql = "SELECT table_schema || '.' || table_name AS name, lower(table_type) AS type FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') ORDER BY 1"
	case "mysql":
		sql = "SELECT table_name AS name, lower(table_type) AS type FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY 1"
	}
	return k.Query(ctx, domain, name, sql)
}

// Query runs sql, read-only, where the database is.
func (k *Keeper) Query(ctx context.Context, domain, name, sql string) (QueryResult, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return QueryResult{}, errors.New("an empty query")
	}
	m, b, err := k.backupFor(domain, name)
	if err != nil {
		return QueryResult{}, err
	}
	dir := k.siteDir(domain)
	spec, container, err := k.target(domain, dir, m, b)
	if err != nil {
		return QueryResult{}, err
	}
	argv, env, err := command(b, false, container == "", domain, dir)
	if err != nil {
		return QueryResult{}, err
	}
	script, err := queryScript(b.Engine)
	if err != nil {
		return QueryResult{}, err
	}
	argv = []string{"sh", "-c", script}
	env["MW_SQL"] = sql
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var stdout limitedBuffer
	stdout.max = maxQueryBytes
	var stderr bytes.Buffer
	start := time.Now()
	if container != "" {
		var kv []string
		for _, key := range sortedKeys(env) {
			kv = append(kv, key+"="+env[key])
		}
		err = k.opts.Docker.ExecStdio(ctx, container, kv, argv, nil, &stdout, &stderr)
	} else {
		err = k.runLocal(ctx, domain, spec, argv, env, &stdout, &stderr)
	}
	res := QueryResult{Millis: time.Since(start).Milliseconds()}
	if err != nil && !stdout.full {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = "the query took more than " + queryTimeout.String()
		}
		return res, &QueryError{Message: msg}
	}
	cols, rows, truncated := parseOutput(b.Engine, stdout.Bytes())
	res.Columns, res.Rows = cols, rows
	res.Truncated = truncated || stdout.full
	return res, nil
}

// QueryError is the database's own refusal: a syntax error, a missing
// table, a write refused. The API answers it as the person's mistake.
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return e.Message }

func queryScript(engine string) (string, error) {
	switch engine {
	case "sqlite":
		return sqliteQuery, nil
	case "postgres":
		return pgQuery, nil
	case "mysql":
		return mysqlQuery, nil
	}
	return "", fmt.Errorf("the browser does not read %s", engine)
}

// sqliteQuery: -readonly opens the file so that no statement can change
// it; the file is checked first, since sqlite3 would make an empty one.
const sqliteQuery = prelude + `if [ ! -f "$MW_FILE" ]; then echo "$MW_FILE: no such file" >&2; exit 1; fi
bin=$(pick sqlite3)
exec "$bin" -readonly -bail -csv -header -cmd ".timeout 3000" "$MW_FILE" "$MW_SQL"
`

// pgQuery: default_transaction_read_only refuses a write in every
// transaction the session opens; statement_timeout ends a query the
// house has stopped waiting for on the server too.
const pgQuery = prelude + pgCommon + `export PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=30000"
bin=$(pick psql)
exec "$bin" "$@" -X -q --csv -v ON_ERROR_STOP=1 -d "${db:-postgres}" -c "$MW_SQL"
`

// mysqlQuery: SET SESSION TRANSACTION READ ONLY before the query, in the
// same session; --batch writes tab-separated rows with a header line.
const mysqlQuery = prelude + mysqlCommon + `bin=$(pick mariadb mysql)
if [ -n "$db" ] && [ "$db" != "*" ]; then set -- "$@" "$db"; fi
exec "$bin" "$@" --batch -e "SET SESSION TRANSACTION READ ONLY; $MW_SQL"
`

// parseOutput reads a client's rows: CSV with a header (sqlite3, psql),
// or MySQL's batch format, tab-separated with \t \n \\ and \0 escaped.
func parseOutput(engine string, out []byte) ([]string, [][]string, bool) {
	var records [][]string
	if engine == "mysql" {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			for i, f := range fields {
				fields[i] = unescapeMySQL(f)
			}
			records = append(records, fields)
		}
	} else {
		r := csv.NewReader(bytes.NewReader(out))
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		for {
			rec, err := r.Read()
			if err != nil {
				break
			}
			records = append(records, rec)
		}
	}
	if len(records) == 0 {
		return []string{}, [][]string{}, false
	}
	cols, rows := records[0], records[1:]
	truncated := false
	if len(rows) > MaxQueryRows {
		rows, truncated = rows[:MaxQueryRows], true
	}
	return cols, rows, truncated
}

func unescapeMySQL(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// limitedBuffer keeps the first max bytes and says it stopped there.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	full bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room < len(p) {
		if room > 0 {
			l.Buffer.Write(p[:room])
		}
		l.full = true
		// Accepted and dropped: an error here would end the command
		// halfway, and what is kept is what is shown.
		return len(p), nil
	}
	return l.Buffer.Write(p)
}

// runLocal runs argv as the site's processes run (its folder, its
// environment, its jail), what a backup of a database outside any
// container does.
func (k *Keeper) runLocal(ctx context.Context, domain string, spec *manifest.Spec, argv []string, env map[string]string, stdout, stderr io.Writer) error {
	code, err := runtime.Exec(ctx, spec, filepath.Join(k.data(), "homes", domain), runtime.ExecOptions{Args: argv, Stdout: stdout, Stderr: stderr, Env: env})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit status %d", code)
	}
	return nil
}
