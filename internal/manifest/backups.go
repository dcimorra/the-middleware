package manifest

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/cron"
)

// Backup is one database of the site that the house copies, on a
// schedule or by hand, compressed, into data/backups/<domain>/<name>/
// (internal/backups). It is the manifest's, like everything a site
// decides: a backup configured in a file of the house's would not move
// with the site's decisions, and the site's agent, which writes site.json,
// could not set one up when asked.
//
// The copy is taken where the database is: inside the container of the
// service or worker that runs it (docker exec, as the container's own
// user, with its own environment, so the image's MARIADB_ROOT_PASSWORD or
// POSTGRES_USER are there), or, for a process, as the site's processes
// run (its folder, its environment, its jail). The house reads the dump's
// standard output and compresses it itself: no gzip is needed where the
// dump runs, and a database user who cannot write the site's folder
// still produces a copy.
type Backup struct {
	// Engine is mysql (MariaDB too), postgres, sqlite, mongodb, or
	// command for anything else (dump and restore written by hand).
	Engine string `json:"engine"`
	// Service is the service or worker whose container (or process)
	// holds the database; empty is the site's own run.
	Service string `json:"service,omitempty"`
	// Database is the one to copy. Empty is the one the container was
	// made with (MARIADB_DATABASE, MYSQL_DATABASE, POSTGRES_DB,
	// MONGO_INITDB_DATABASE) and, without one, every database; "*" is
	// every database.
	Database string `json:"database,omitempty"`
	// File is the SQLite database, a path inside the folder.
	File string `json:"file,omitempty"`
	// Host and Port reach a server the dump does not find on its own:
	// empty inside a container is its local socket; from a process,
	// 127.0.0.1 (a jail has no /run, where the socket lives).
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// User is who the dump connects as; empty is the image's root user
	// (root, POSTGRES_USER or postgres, MONGO_INITDB_ROOT_USERNAME).
	User string `json:"user,omitempty"`
	// PasswordEnv is the name of the variable holding the password, in
	// the container's environment or the site's (.env): the password
	// itself is never written in the manifest, which travels and is kept
	// in a history. Empty is the image's own (MARIADB_ROOT_PASSWORD…).
	PasswordEnv string `json:"password_env,omitempty"`
	// Dump and Restore are the command engine's: Dump writes the copy to
	// its standard output, Restore reads it from its standard input.
	Dump    []string `json:"dump,omitempty"`
	Restore []string `json:"restore,omitempty"`
	// Schedule is five-field cron; empty is by hand only.
	Schedule string `json:"schedule,omitempty"`
	// Timezone reads the schedule in that zone; empty is the machine's.
	Timezone string `json:"timezone,omitempty"`
	// Keep is how many copies are kept, the newest; 0 is 7.
	Keep int `json:"keep,omitempty"`
	// Timeout bounds a copy and a restore; 0 is an hour.
	Timeout  Duration `json:"timeout,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// The engines a backup knows.
var BackupEngines = []string{"mysql", "postgres", "sqlite", "mongodb", "command"}

// DefaultBackupKeep and DefaultBackupTimeout are what an empty keep and
// timeout mean.
const (
	DefaultBackupKeep    = 7
	DefaultBackupTimeout = time.Hour
	maxBackupKeep        = 1000
)

// KeepCopies is how many copies are kept.
func (b *Backup) KeepCopies() int {
	if b.Keep <= 0 {
		return DefaultBackupKeep
	}
	return b.Keep
}

// TimeoutOrDefault bounds a copy or a restore.
func (b *Backup) TimeoutOrDefault() time.Duration {
	if b.Timeout <= 0 {
		return DefaultBackupTimeout
	}
	return time.Duration(b.Timeout)
}

// Location is the zone the schedule is read in.
func (b *Backup) Location() *time.Location {
	if b.Timezone == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(b.Timezone); err == nil {
		return loc
	}
	return time.Local
}

// backupNameRe is a backup's name: a folder under data/backups and part
// of a URL.
var backupNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidBackupName reports whether s can name a backup.
func ValidBackupName(s string) bool { return backupNameRe.MatchString(s) }

// BackupNames are the backups, sorted.
func (m *Manifest) BackupNames() []string {
	out := make([]string, 0, len(m.Backups))
	for n := range m.Backups {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hostRe is what a host may be: a name or an address, nothing a shell or
// a dump's options would read as more.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// dbWordRe is a database or a user name as the dump is given it: they
// reach it as variables, never as script, but a name that begins with a
// dash would be an option.
var dbWordRe = regexp.MustCompile(`^[A-Za-z0-9_$][A-Za-z0-9_.$@-]{0,127}$`)

func (m *Manifest) validBackups() error {
	for _, name := range m.BackupNames() {
		b := m.Backups[name]
		f := "backups." + name
		if !ValidBackupName(name) {
			return fmt.Errorf("backups: %q is not a name (a-z, 0-9 and -, up to 32)", name)
		}
		if b == nil {
			return fmt.Errorf("%s: empty", f)
		}
		switch b.Engine {
		case "mysql", "postgres", "mongodb":
			if len(b.Dump) > 0 || len(b.Restore) > 0 {
				return fmt.Errorf("%s: dump and restore are the command engine's; %s knows its own", f, b.Engine)
			}
			if b.File != "" {
				return fmt.Errorf("%s.file: only sqlite reads a file", f)
			}
		case "sqlite":
			if b.File == "" {
				return fmt.Errorf("%s.file: sqlite needs the database's file, a path inside the folder", f)
			}
			if c := path.Clean(b.File); c != b.File || path.IsAbs(b.File) || c == "." || c == ".." || strings.HasPrefix(c, "../") || c == FileName {
				return fmt.Errorf("%s.file: %q is not a file inside the folder (relative, clean, no ..)", f, b.File)
			}
			if b.Service != "" {
				return fmt.Errorf("%s.service: sqlite copies the file in the folder, where the site's processes run; leave service out", f)
			}
			if b.Database != "" || b.Host != "" || b.Port != 0 || b.User != "" || b.PasswordEnv != "" {
				return fmt.Errorf("%s: sqlite has a file, not a server: database, host, port, user and password_env do not apply", f)
			}
			if len(b.Dump) > 0 || len(b.Restore) > 0 {
				return fmt.Errorf("%s: dump and restore are the command engine's", f)
			}
		case "command":
			if len(b.Dump) == 0 || strings.TrimSpace(b.Dump[0]) == "" {
				return fmt.Errorf("%s.dump: the command engine needs the command that writes the copy to its standard output", f)
			}
			if len(b.Restore) > 0 && strings.TrimSpace(b.Restore[0]) == "" {
				return fmt.Errorf("%s.restore: empty", f)
			}
			if b.File != "" || b.Database != "" || b.Host != "" || b.Port != 0 || b.User != "" || b.PasswordEnv != "" {
				return fmt.Errorf("%s: the command engine takes dump and restore only; put the rest in them", f)
			}
		case "":
			return fmt.Errorf("%s.engine: empty; one of %s", f, strings.Join(BackupEngines, ", "))
		default:
			return fmt.Errorf("%s.engine: %q is not one of %s", f, b.Engine, strings.Join(BackupEngines, ", "))
		}
		if b.Service == "" && len(m.Services) > 0 && b.Engine != "sqlite" {
			// The site's own name is the web's service, and a dump run
			// there would look for a database that is not in it.
			return fmt.Errorf("%s.service: a site with services names the one that holds the database (%s)", f, strings.Join(m.ServiceNames(), ", "))
		}
		if b.Service != "" {
			_, isService := m.Services[b.Service]
			_, isWorker := m.Workers[b.Service]
			if !isService && !isWorker {
				return fmt.Errorf("%s.service: %q is not a service nor a worker of this site", f, b.Service)
			}
		}
		if b.Database != "" && b.Database != "*" && !dbWordRe.MatchString(b.Database) {
			return fmt.Errorf("%s.database: %q is not a database's name", f, b.Database)
		}
		if b.User != "" && !dbWordRe.MatchString(b.User) {
			return fmt.Errorf("%s.user: %q is not a user's name", f, b.User)
		}
		if b.Host != "" && !hostRe.MatchString(b.Host) {
			return fmt.Errorf("%s.host: %q is not a host", f, b.Host)
		}
		if b.Port < 0 || b.Port > 65535 {
			return fmt.Errorf("%s.port: %d is not a port", f, b.Port)
		}
		if b.PasswordEnv != "" && !validEnvKey(b.PasswordEnv) {
			return fmt.Errorf("%s.password_env: %q is not a variable's name (the password itself never goes in the manifest)", f, b.PasswordEnv)
		}
		if b.Schedule != "" {
			if _, err := cron.Parse(b.Schedule); err != nil {
				return fmt.Errorf("%s.schedule: %w", f, err)
			}
		}
		if b.Timezone != "" {
			if _, err := time.LoadLocation(b.Timezone); err != nil {
				return fmt.Errorf("%s.timezone: %q is not a zone (Europe/Madrid, UTC…)", f, b.Timezone)
			}
		}
		if b.Keep < 0 || b.Keep > maxBackupKeep {
			return fmt.Errorf("%s.keep: %d is not between 1 and %d (0 is %d)", f, b.Keep, maxBackupKeep, DefaultBackupKeep)
		}
		if b.Timeout < 0 {
			return fmt.Errorf("%s.timeout: negative", f)
		}
	}
	return nil
}
