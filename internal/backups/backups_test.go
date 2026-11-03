package backups

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixture is an installation with one site whose manifest is given.
type fixture struct {
	root, dir string
	k         *Keeper
	mu        sync.Mutex
	now       time.Time
}

func newFixture(t *testing.T, manifest string) *fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sites", "shop.example")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: root, dir: dir, now: time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)}
	// Every reading of the clock is a second later: two copies are two
	// names.
	f.k = New(Options{Root: root, Now: func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.now = f.now.Add(time.Second)
		return f.now
	}})
	return f
}

func (f *fixture) write(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// gunzip is a copy's content, read as a person who downloaded it would.
func gunzip(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const commandSite = `{
  "version": 2,
  "backups": {
    "data": {
      "engine": "command",
      "dump": ["sh", "-c", "cat data.txt"],
      "restore": ["sh", "-c", "cat > data.txt"],
      "keep": 2
    }
  }
}`

// The whole round: copies taken and compressed, the oldest dropped past
// keep, one put back after a copy of what was there.
func TestBackupKeepAndRestore(t *testing.T) {
	f := newFixture(t, commandSite)
	ctx := context.Background()
	f.write(t, "data.txt", "one")
	first, err := f.k.Backup(ctx, "shop.example", "data", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(first.ID, "-manual.dump.gz") {
		t.Fatalf("copy %q", first.ID)
	}
	p, err := f.k.Path("shop.example", "data", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, p); got != "one" {
		t.Fatalf("copy holds %q", got)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode %v: a database's content is the house's only", info.Mode().Perm())
	}
	f.write(t, "data.txt", "two")
	if _, err := f.k.Backup(ctx, "shop.example", "data", "schedule"); err != nil {
		t.Fatal(err)
	}
	f.write(t, "data.txt", "three")
	third, err := f.k.Backup(ctx, "shop.example", "data", "schedule")
	if err != nil {
		t.Fatal(err)
	}
	copies := f.k.Copies("shop.example", "data")
	if len(copies) != 2 || copies[0].ID != third.ID {
		t.Fatalf("keep 2 left %+v", copies)
	}
	if _, err := f.k.Path("shop.example", "data", first.ID); err == nil {
		t.Fatal("the oldest copy is still there past keep")
	}

	f.write(t, "data.txt", "now")
	second := copies[1]
	if _, err := f.k.Restore(ctx, "shop.example", "data", second.ID, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "data.txt"); got != "two" {
		t.Fatalf("restored %q, want two", got)
	}
	var pre []Copy
	for _, c := range f.k.Copies("shop.example", "data") {
		if c.Kind == KindPreRestore {
			pre = append(pre, c)
		}
	}
	if len(pre) != 1 {
		t.Fatalf("pre-restore copies %+v", pre)
	}
	if got := gunzip(t, filepath.Join(f.k.Dir("shop.example", "data"), pre[0].ID)); got != "now" {
		t.Fatalf("the copy taken before the restore holds %q, want what was there (now)", got)
	}
	list, err := f.k.List("shop.example")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	st := list[0]
	if st.Last == nil || !st.Last.OK || st.LastRestore == nil || !st.LastRestore.OK || st.LastRestore.Copy != second.ID {
		t.Fatalf("status %+v last %+v restore %+v", st, st.Last, st.LastRestore)
	}
	if log, _ := f.k.Log("shop.example", "data", 0); !strings.Contains(log, "restore of "+second.ID) {
		t.Fatalf("log:\n%s", log)
	}
}

// A dump that fails, or writes nothing, leaves no copy and no half file,
// and says why with what the program said.
func TestBackupFailureLeavesNothing(t *testing.T) {
	f := newFixture(t, `{
  "version": 2,
  "backups": {
    "broken": { "engine": "command", "dump": ["sh", "-c", "echo 'Access denied for user app' >&2; exit 3"] },
    "empty": { "engine": "command", "dump": ["true"] }
  }
}`)
	ctx := context.Background()
	_, err := f.k.Backup(ctx, "shop.example", "broken", "manual")
	if err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("error %v, want the program's words", err)
	}
	if _, err := f.k.Backup(ctx, "shop.example", "empty", "manual"); err == nil || !strings.Contains(err.Error(), "without writing anything") {
		t.Fatalf("an empty dump: %v", err)
	}
	for _, name := range []string{"broken", "empty"} {
		ents, _ := os.ReadDir(f.k.Dir("shop.example", name))
		for _, e := range ents {
			if e.Name() != "state.json" {
				t.Fatalf("%s left %s", name, e.Name())
			}
		}
	}
	list, _ := f.k.List("shop.example")
	for _, st := range list {
		if st.Last == nil || st.Last.OK || st.Last.Error == "" {
			t.Fatalf("%s: last %+v", st.Name, st.Last)
		}
	}
}

// A restore whose first copy (of the database as it is) fails restores
// nothing, unless told to go without it.
func TestRestoreNeedsTheSafetyCopy(t *testing.T) {
	f := newFixture(t, commandSite)
	ctx := context.Background()
	f.write(t, "data.txt", "good")
	c, err := f.k.Backup(ctx, "shop.example", "data", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "data.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.k.Restore(ctx, "shop.example", "data", c.ID, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("restore over a database that cannot be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "data.txt")); err == nil {
		t.Fatal("restored although the safety copy failed")
	}
	if _, err := f.k.Restore(ctx, "shop.example", "data", c.ID, RestoreOptions{NoSafetyCopy: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "data.txt"); got != "good" {
		t.Fatalf("restored %q", got)
	}
}

// Ids are names of copies and nothing else: no path, no state file.
func TestCopyIDs(t *testing.T) {
	f := newFixture(t, commandSite)
	for _, id := range []string{"state.json", "../site.json", "20261103-030000-manual.dump.gz/../x", "20261103-030000-manual.sql"} {
		if _, err := f.k.Path("shop.example", "data", id); err == nil {
			t.Fatalf("%q taken for a copy", id)
		}
		if err := f.k.Delete("shop.example", "data", id); err == nil {
			t.Fatalf("%q deleted", id)
		}
	}
}

// The schedule: due once its minute passed since the last look, in its
// zone; not when it or its site is off.
func TestDue(t *testing.T) {
	f := newFixture(t, `{
  "version": 2,
  "backups": {
    "night": { "engine": "command", "dump": ["true"], "schedule": "0 3 * * *" },
    "madrid": { "engine": "command", "dump": ["true"], "schedule": "0 3 * * *", "timezone": "Europe/Madrid" },
    "off": { "engine": "command", "dump": ["true"], "schedule": "0 3 * * *", "disabled": true },
    "hand": { "engine": "command", "dump": ["true"] }
  }
}`)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.UTC) }
	got := f.k.due(at(2, 59), at(3, 0))
	if len(got) != 1 || got[0][1] != "night" {
		t.Fatalf("due at 03:00 UTC: %v", got)
	}
	// 03:00 in Madrid is 02:00 UTC in November.
	if got := f.k.due(at(1, 59), at(2, 0)); len(got) != 1 || got[0][1] != "madrid" {
		t.Fatalf("due at 02:00 UTC: %v", got)
	}
	if got := f.k.due(at(3, 0), at(3, 1)); len(got) != 0 {
		t.Fatalf("due twice: %v", got)
	}
	f.write(t, "site.json", `{"version": 2, "disabled": true, "backups": {"night": {"engine": "command", "dump": ["true"], "schedule": "0 3 * * *"}}}`)
	if got := f.k.due(at(2, 59), at(3, 0)); len(got) != 0 {
		t.Fatalf("a stopped site's backups ran: %v", got)
	}
}

// fakeBin puts programs that say how they were called on PATH.
func fakeBin(t *testing.T, scripts map[string]string) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// The mysql engine as it runs from a process: the client the machine
// has, the password from the variable named (never an argument), the
// database's name as a name, 127.0.0.1 because a jail has no socket.
func TestMySQLFromAProcess(t *testing.T) {
	fakeBin(t, map[string]string{
		"mysqldump": `echo "args: $*"; echo "pwd: $MYSQL_PWD"`,
		"mysql":     `echo "restore args: $*" >&2; echo "pwd: $MYSQL_PWD" >&2; cat > "$MIDDLEWARE_SITE_DIR/restored.sql"`,
	})
	f := newFixture(t, `{
  "version": 2,
  "run": { "command": ["sleep", "1000"], "env": { "DB_PASS": "s3cret" } },
  "backups": {
    "db": { "engine": "mysql", "database": "shop$x", "user": "app", "password_env": "DB_PASS", "port": 3307 }
  }
}`)
	ctx := context.Background()
	c, err := f.k.Backup(ctx, "shop.example", "db", "manual")
	if err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	p, _ := f.k.Path("shop.example", "db", c.ID)
	got := gunzip(t, p)
	for _, want := range []string{"-u app", "-h 127.0.0.1 --protocol=tcp", "-P 3307", "--single-transaction", "--databases shop$x", "pwd: s3cret"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dump said:\n%s\nwant %q", got, want)
		}
	}
	if strings.Contains(strings.SplitN(got, "\n", 2)[0], "s3cret") {
		t.Fatal("the password went as an argument")
	}
	if !strings.HasSuffix(c.ID, ".sql.gz") {
		t.Fatalf("id %s", c.ID)
	}
	if _, err := f.k.Restore(ctx, "shop.example", "db", c.ID, RestoreOptions{}); err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	if f.read(t, "restored.sql") != got {
		t.Fatal("the restore did not read the copy")
	}
	log, _ := f.k.Log("shop.example", "db", 0)
	if !strings.Contains(log, "pwd: s3cret") {
		t.Fatalf("restore log:\n%s", log)
	}
}

// postgres from a process: pg_dump of one database with what drops
// before making, psql in one transaction.
func TestPostgresFromAProcess(t *testing.T) {
	fakeBin(t, map[string]string{
		"pg_dump": `echo "args: $*"; echo "pw: $PGPASSWORD"`,
		"psql":    `echo "psql: $*" >&2; cat >/dev/null`,
	})
	f := newFixture(t, `{
  "version": 2,
  "run": { "command": ["sleep", "1000"], "env": { "PGPASS": "pg" } },
  "backups": { "pg": { "engine": "postgres", "database": "shop", "user": "app", "password_env": "PGPASS" } }
}`)
	ctx := context.Background()
	c, err := f.k.Backup(ctx, "shop.example", "pg", "manual")
	if err != nil {
		log, _ := f.k.Log("shop.example", "pg", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	p, _ := f.k.Path("shop.example", "pg", c.ID)
	got := gunzip(t, p)
	for _, want := range []string{"-U app", "-h 127.0.0.1", "--clean --if-exists shop", "pw: pg"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dump said:\n%s\nwant %q", got, want)
		}
	}
	if _, err := f.k.Restore(ctx, "shop.example", "pg", c.ID, RestoreOptions{NoSafetyCopy: true}); err != nil {
		t.Fatal(err)
	}
	log, _ := f.k.Log("shop.example", "pg", 0)
	if !strings.Contains(log, "ON_ERROR_STOP=1 --single-transaction -d shop") {
		t.Fatalf("restore log:\n%s", log)
	}
}

// A client that is not installed is named, not an exit code.
func TestMissingClient(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := exec.LookPath("mongodump"); err == nil {
		t.Skip("mongodump is installed here")
	}
	f := newFixture(t, `{"version": 2, "backups": {"m": {"engine": "mongodb"}}}`)
	_, err := f.k.Backup(context.Background(), "shop.example", "m", "manual")
	if err == nil || !strings.Contains(err.Error(), "none of mongodump is installed") {
		t.Fatalf("%v", err)
	}
}

// SQLite for real, where sqlite3 is installed: the copy is the
// database's SQL, and a restore replaces the file and its journal.
func TestSQLite(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	f := newFixture(t, `{"version": 2, "backups": {"app": {"engine": "sqlite", "file": "data/app.db"}}}`)
	db := filepath.Join(f.dir, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	sql := func(q string) string {
		out, err := exec.Command("sqlite3", db, q).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", q, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sql("PRAGMA journal_mode=WAL; CREATE TABLE t(v TEXT); INSERT INTO t VALUES ('kept');")
	ctx := context.Background()
	c, err := f.k.Backup(ctx, "shop.example", "app", "manual")
	if err != nil {
		log, _ := f.k.Log("shop.example", "app", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	p, _ := f.k.Path("shop.example", "app", c.ID)
	if got := gunzip(t, p); !strings.Contains(got, "INSERT INTO t VALUES('kept')") {
		t.Fatalf("copy:\n%s", got)
	}
	sql("DELETE FROM t; INSERT INTO t VALUES ('lost');")
	if _, err := f.k.Restore(ctx, "shop.example", "app", c.ID, RestoreOptions{}); err != nil {
		log, _ := f.k.Log("shop.example", "app", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	if got := sql("SELECT v FROM t"); got != "kept" {
		t.Fatalf("after the restore: %q", got)
	}
	if _, err := os.Stat(db + ".restoring"); err == nil {
		t.Fatal("the restore left its file")
	}
	// A file that is not there is not copied as an empty database.
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if _, err := f.k.Backup(ctx, "shop.example", "app", "manual"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("copy of a missing file: %v", err)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("the copy made an empty database where there was none")
	}
}

// One copy at a time per backup.
func TestBusy(t *testing.T) {
	f := newFixture(t, `{"version": 2, "backups": {"slow": {"engine": "command", "dump": ["sh", "-c", "sleep 1; echo x"]}}}`)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := f.k.Backup(ctx, "shop.example", "slow", "manual")
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.k.Running("shop.example", "slow") == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.k.Backup(ctx, "shop.example", "slow", "manual"); err != ErrBusy {
		t.Fatalf("a second copy at once: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
