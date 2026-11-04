package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// fakeDocker is a docker CLI that writes down every call and answers what
// Start needs: the commands a service's container is made with are the
// walls and the network, and they are read here, not hoped for.
func fakeServicesDocker(t *testing.T, arch string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
case "$1 $2 $3" in
  "network inspect -f") echo "172.30.0.1 " ; exit 0 ;;
esac
case "$1 $2" in
  "network inspect") exit 1 ;;
  "image inspect") echo ` + arch + ` ;;
  "buildx version") echo v0.17 ;;
  "buildx build") echo "#1 building" ; echo "#2 DONE" ;;
  "info --format") echo "[name=seccomp]" ;;
esac
case "$1" in
  run) echo 0123456789abcdef ;;
  wait|logs) sleep 2 ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func callsOf(t *testing.T, path, verb string) []string {
	t.Helper()
	data, _ := os.ReadFile(path)
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, verb) {
			out = append(out, l)
		}
	}
	return out
}

func TestAServicesContainerJoinsItsSitesNetworkWithItsOwnPortInside(t *testing.T) {
	bin, calls := fakeServicesDocker(t, goruntime.GOARCH)
	site := t.TempDir()
	os.MkdirAll(filepath.Join(site, "deploy", "next"), 0o755)
	os.WriteFile(filepath.Join(site, "deploy", "next", "Dockerfile"), []byte("FROM node:24-slim\n"), 0o644)
	m, err := manifest.Parse([]byte(`{"version":2,"services":{
		"next":{"run":{"runtime":"docker","docker":{"build":"deploy/next/Dockerfile","memory":"768m"},"command":["npm","start"],
		        "env":{"DB":"$SERVICE_db_HOST:$SERVICE_db_PORT"}},"domains":["bakery.test"]},
		"db":{"run":{"runtime":"docker","docker":{"image":"mariadb:11.4","port":3306}},"internal":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	addr := func(target string, fromContainer bool) (string, int, bool) {
		if !fromContainer {
			t.Errorf("a container asked for %s's machine address", target)
		}
		return target, 3306, true
	}
	d := Docker{Bin: bin, House: "/srv/test"}
	logs := filepath.Join(t.TempDir(), "logs", "bakery.test")
	for _, name := range []string{"next", "db"} {
		spec, err := manifest.ServiceSpec("bakery.test", site, name, m, addr)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := d.Start(context.Background(), spec, 41234, filepath.Join(logs, name+".log"), t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = inst
	}
	runs := callsOf(t, calls, "run ")
	if len(runs) != 2 {
		t.Fatalf("runs: %q", runs)
	}
	next, db := runs[0], runs[1]
	for _, want := range []string{"--network mw-bakery-test-net", "--network-alias next", "-p 127.0.0.1:41234:8080", "-e PORT=8080", "-e DB=db:3306", "-e MIDDLEWARE_SERVICE=next", "--cap-drop ALL", "--memory 768m"} {
		if !strings.Contains(next, want) {
			t.Errorf("next's run lacks %q: %s", want, next)
		}
	}
	if strings.Contains(db, " -p ") {
		t.Errorf("an internal database is published on the machine: %s", db)
	}
	if !strings.Contains(db, "--network-alias db") || !strings.Contains(db, "-e PORT=3306") {
		t.Errorf("db's run: %s", db)
	}
	if b := callsOf(t, calls, "buildx build"); len(b) != 1 || !strings.Contains(b[0], "--load") || !strings.HasSuffix(b[0], " "+site) {
		t.Errorf("the image is built with BuildKit, the site's folder its context: %q", b)
	}
	if n := callsOf(t, calls, "network create"); len(n) == 0 || !strings.Contains(n[0], "middleware.site=bakery.test") {
		t.Errorf("the site's network is made and labelled: %q", n)
	}
	if log, _ := os.ReadFile(BuildLog(filepath.Join(logs, "next.log"), "next")); !strings.Contains(string(log), "DONE") {
		t.Errorf("the build's output is in its log: %q", log)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestAnImageForAnotherMachineIsRefusedBeforeItRuns(t *testing.T) {
	other := "amd64"
	if goruntime.GOARCH == "amd64" {
		other = "arm64"
	}
	bin, calls := fakeServicesDocker(t, other)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: t.TempDir(), Runtime: "docker", Docker: &manifest.Docker{Image: "acme/app:1"}}
	_, err := Docker{Bin: bin}.Start(context.Background(), spec, 41235, filepath.Join(t.TempDir(), "site.log"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "built for "+other) || !strings.Contains(err.Error(), "COPY --from") {
		t.Fatalf("err %v", err)
	}
	if len(callsOf(t, calls, "run ")) != 0 {
		t.Error("it ran anyway")
	}
}

// A command run in a service's container is on the site's network, so a
// migration reaches "db" by name as the service does.
func TestACommandInAServiceJoinsTheSitesNetwork(t *testing.T) {
	bin, calls := fakeServicesDocker(t, goruntime.GOARCH)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "bakery.test/drupal", Service: "drupal", SiteDir: dir, Cwd: dir, Runtime: "docker",
		Docker: &manifest.Docker{Image: "drupal:11.1"}, Network: manifest.DockerNetwork("bakery.test"), Env: map[string]string{}}
	code, err := Exec(context.Background(), spec, filepath.Join(dir, "home"), ExecOptions{Args: []string{"drush", "updb"}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil || code != 0 {
		t.Fatalf("exec: %d %v", code, err)
	}
	run := callsOf(t, calls, "run")
	if len(run) != 1 || !strings.Contains(run[0], "--network mw-bakery-test-net") || !strings.HasSuffix(run[0], "drupal:11.1 drush updb") {
		t.Fatalf("run %q", run)
	}
	if len(callsOf(t, calls, "network")) == 0 {
		t.Error("the network was not made sure of first")
	}
}

// On a replica, a container reaches the services kept on the primary by
// their names, which point at the machine on the site's network, where
// the house passes them through the mesh (internal/cluster).
func TestAReplicasContainerFindsTheKeptServicesOnTheMachine(t *testing.T) {
	bin, calls := fakeServicesDocker(t, goruntime.GOARCH)
	site := t.TempDir()
	m, err := manifest.Parse([]byte(`{"version":2,"services":{
		"web":{"run":{"runtime":"docker","docker":{"image":"nginx:1.27"},"env":{"DB":"$SERVICE_db_HOST"}},"domains":["bakery.test"]},
		"db":{"run":{"runtime":"docker","docker":{"image":"mariadb:11.4","port":3306,"volumes":["data:/var/lib/mysql"]}},"internal":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := manifest.ServiceSpec("bakery.test", site, "web", m, func(target string, _ bool) (string, int, bool) { return target, 3306, true })
	if err != nil {
		t.Fatal(err)
	}
	spec.AddHosts = []string{"db"}
	d := Docker{Bin: bin, House: "/srv/test"}
	if _, err := d.Start(context.Background(), spec, 41235, filepath.Join(t.TempDir(), "web.log"), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	runs := callsOf(t, calls, "run ")
	if len(runs) != 1 || !strings.Contains(runs[0], "--add-host db:172.30.0.1") {
		t.Fatalf("runs: %q", runs)
	}
}
