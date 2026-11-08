package sites

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// TestTheContainerTemplatesAnswerBehindTheWalls builds and runs each
// container template the way the house does (runtime.Docker.Start: no
// capabilities, no new privileges, the folder mounted at its own path)
// and as a user that is not root, because that is the case a template
// written on a root laptop gets wrong: Apache could not drop to www-data,
// and a Django that wrote its database beside the code could not write it.
// It downloads images and packages, so it only runs with DOCKER=1.
func TestTheContainerTemplatesAnswerBehindTheWalls(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 builds and runs the container templates")
	}
	for i, tc := range []struct{ name, path, want string }{
		{"fastapi", "/", "served by middleware"},
		{"django", "/", "served by middleware"},
		{"php", "/", "Running on PHP 8.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			domain := tc.name + ".test"
			site := filepath.Join(root, "sites", domain)
			m, files := templates[tc.name](domain)
			for rel, body := range files {
				p := filepath.Join(site, rel)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Someone else's uid, with the folder open to it as the
			// supervisor's own folder is to the supervisor.
			filepath.Walk(site, func(p string, _ os.FileInfo, _ error) error { return os.Chmod(p, 0o777) })
			m.Run.Docker.User = "65534:65534"
			spec, err := manifest.Resolve(domain, site, m)
			if err != nil {
				t.Fatal(err)
			}
			spec.Revision = "live" + strconv.Itoa(i)
			d := runtime.Docker{House: filepath.Join(root, "sites")}
			port := 47921 + i
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(firstBuild))
			defer cancel()
			inst, err := d.Start(ctx, spec, port, filepath.Join(root, "site.log"), filepath.Join(root, "home"))
			if err != nil {
				t.Fatal(err)
			}
			name := inst.State().Container
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
			if err := runtime.WaitReady(ctx, inst, *m.Run.Health, 3*time.Minute); err != nil {
				out, _ := exec.Command("docker", "logs", name).CombinedOutput()
				t.Fatalf("not ready: %v\n%s", err, out)
			}
			resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), tc.want) {
				out, _ := exec.Command("docker", "logs", name).CombinedOutput()
				t.Fatalf("%d %q\n%s", resp.StatusCode, body, out)
			}
		})
	}
}
