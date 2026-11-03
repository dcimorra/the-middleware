package backups

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// TestMariaDBInItsContainer copies and restores a real MariaDB through
// docker exec, the way a site's database worker or service is copied:
// as the container's user, with the image's own root password, from its
// local socket. DOCKER=1 runs it (it pulls mariadb:11.4 when missing).
func TestMariaDBInItsContainer(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real container")
	}
	image := os.Getenv("DOCKER_DB_IMAGE")
	if image == "" {
		image = "mariadb:11.4"
	}
	name := "mw-backups-test-" + strings.ToLower(t.Name())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-e", "MARIADB_ROOT_PASSWORD=r00t", "-e", "MARIADB_DATABASE=shop", image).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	sql := func(q string) string {
		out, err := exec.Command("docker", "exec", name, "mariadb", "-uroot", "-pr00t", "-N", "shop", "-e", q).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", q, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for exec.Command("docker", "exec", name, "healthcheck.sh", "--connect", "--innodb_initialized").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("mariadb did not come up")
		}
		time.Sleep(time.Second)
	}
	sql("CREATE TABLE t (v VARCHAR(20)); INSERT INTO t VALUES ('kept')")

	f := newFixture(t, `{
  "version": 2,
  "workers": { "db": { "command": ["mariadbd"], "runtime": "docker", "docker": { "image": "`+image+`" } } },
  "backups": { "db": { "engine": "mysql", "service": "db" } }
}`)
	if err := fsx.WriteJSON(runtime.StatePath(filepath.Join(f.root, "data"), "shop.example/db"), runtime.State{Domain: "shop.example/db", Runtime: "docker", Container: name}, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := f.k.Backup(ctx, "shop.example", "db", "manual")
	if err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	p, _ := f.k.Path("shop.example", "db", c.ID)
	if got := gunzip(t, p); !strings.Contains(got, "INSERT INTO `t` VALUES") || !strings.Contains(got, "CREATE DATABASE") {
		t.Fatalf("copy:\n%.2000s", got)
	}
	sql("DELETE FROM t; INSERT INTO t VALUES ('lost')")
	if _, err := f.k.Restore(ctx, "shop.example", "db", c.ID, RestoreOptions{}); err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	if got := sql("SELECT v FROM t"); got != "kept" {
		t.Fatalf("after the restore: %q", got)
	}
}

// TestPostgresInItsContainer is the same with PostgreSQL: the image's
// POSTGRES_USER and POSTGRES_DB, through its local socket, and a restore
// in one transaction over the database as it is.
func TestPostgresInItsContainer(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real container")
	}
	image := os.Getenv("DOCKER_PG_IMAGE")
	if image == "" {
		image = "postgres:17"
	}
	name := "mw-backups-test-" + strings.ToLower(t.Name())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-e", "POSTGRES_PASSWORD=pw", "-e", "POSTGRES_USER=app", "-e", "POSTGRES_DB=shop", image).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	sql := func(q string) string {
		out, err := exec.Command("docker", "exec", name, "psql", "-U", "app", "-d", "shop", "-tA", "-c", q).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", q, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for exec.Command("docker", "exec", name, "psql", "-U", "app", "-d", "shop", "-c", "select 1").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("postgres did not come up")
		}
		time.Sleep(time.Second)
	}
	sql("CREATE TABLE t (v text); INSERT INTO t VALUES ('kept')")

	f := newFixture(t, `{
  "version": 2,
  "workers": { "db": { "command": ["postgres"], "runtime": "docker", "docker": { "image": "`+image+`" } } },
  "backups": { "db": { "engine": "postgres", "service": "db" } }
}`)
	if err := fsx.WriteJSON(runtime.StatePath(filepath.Join(f.root, "data"), "shop.example/db"), runtime.State{Domain: "shop.example/db", Runtime: "docker", Container: name}, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := f.k.Backup(ctx, "shop.example", "db", "manual")
	if err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	sql("DELETE FROM t; INSERT INTO t VALUES ('lost'); CREATE TABLE extra (x int)")
	if _, err := f.k.Restore(ctx, "shop.example", "db", c.ID, RestoreOptions{}); err != nil {
		log, _ := f.k.Log("shop.example", "db", 0)
		t.Fatalf("%v\n%s", err, log)
	}
	if got := sql("SELECT v FROM t"); got != "kept" {
		t.Fatalf("after the restore: %q", got)
	}
}
