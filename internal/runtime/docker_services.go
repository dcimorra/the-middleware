package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// siteOf is the site a spec's domain belongs to: "shop.test/db" is
// shop.test's.
func siteOf(domain string) string {
	site, _, _ := strings.Cut(domain, "/")
	return site
}

// ensureNetwork makes a site's network the first time one of its services
// needs it. A bridge of its own: on docker's default one every container
// of every site reached every other by address, and a name there resolves
// to nothing. Labelled, so whose it is can be told.
func (d Docker) ensureNetwork(ctx context.Context, name, site string) error {
	if exec.CommandContext(ctx, d.bin(), "network", "inspect", name).Run() == nil {
		return nil
	}
	args := []string{"network", "create", "--driver", "bridge", "--label", "middleware.site=" + site}
	if d.House != "" {
		args = append(args, "--label", HouseLabel+"="+d.House)
	}
	out, err := exec.CommandContext(ctx, d.bin(), append(args, name)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("docker network create %s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// NetworkGateway is the machine's address on a docker network: what a
// container on it reaches the machine at.
func (d Docker) NetworkGateway(ctx context.Context, name string) (string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), "network", "inspect", "-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", name).Output()
	if err != nil {
		return "", fmt.Errorf("docker network inspect %s: %w", name, err)
	}
	for _, f := range strings.Fields(string(out)) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("docker network %s has no IPv4 gateway", name)
}

// ServiceIP is a running service container's address on the site's
// network, found by its label (the site and the service).
func (d Docker) ServiceIP(ctx context.Context, site, service, network string) (string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-q", "--filter", "label="+domainLabel+"="+site+"/"+service).Output()
	if err != nil {
		return "", fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return "", fmt.Errorf("no container of %s/%s is running", site, service)
	}
	ip, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{with index .NetworkSettings.Networks \""+network+"\"}}{{.IPAddress}}{{end}}", ids[0]).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}
	if a := strings.TrimSpace(string(ip)); net.ParseIP(a) != nil {
		return a, nil
	}
	return "", fmt.Errorf("the container of %s/%s is not on %s", site, service, network)
}

// BuildLog is where a service's image build writes, next to its log.
func BuildLog(logFile, service string) string {
	return filepath.Join(filepath.Dir(logFile), "build-"+service+".log")
}

var (
	buildxOnce sync.Once
	buildxOK   bool
)

// hasBuildx says whether the daemon's CLI has BuildKit's front end. The
// legacy builder it replaces is deprecated and prints so on every build.
func (d Docker) hasBuildx(ctx context.Context) bool {
	buildxOnce.Do(func() {
		buildxOK = exec.CommandContext(ctx, d.bin(), "buildx", "version").Run() == nil
	})
	return buildxOK
}

// buildService builds a service's image with BuildKit when the machine
// has it, its whole output in the service's build log (site logs
// --service x --build) rather than held in memory until it ends. The
// context is the site's folder and its .dockerignore is read, as BuildKit
// also reads a <Dockerfile>.dockerignore next to the Dockerfile. A failure
// says the last lines of the log: the reason a build failed is in them.
//
// The output goes to the file itself, not through a pipe this process
// reads: a build that outlives a supervisor restart would otherwise lose
// its reader.
func (d Docker) buildService(ctx context.Context, spec *manifest.Spec, image, logFile string) error {
	path := BuildLog(logFile, spec.Service)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	dockerfile := filepath.Join(spec.SiteDir, spec.Docker.Build)
	args := []string{"build", "-t", image, "-f", dockerfile, spec.SiteDir}
	if d.hasBuildx(ctx) {
		args = append([]string{"buildx", "build", "--load", "--progress", "plain"}, args[1:]...)
	} else {
		fmt.Fprintln(f, "middleware: docker buildx is not installed, building with the legacy builder (System → the house's requirements installs it)")
	}
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build of %s: %w (the end of %s: %s)", spec.Service, err, path, lastLines(path, 12))
	}
	return nil
}

// checkArch refuses an image made for another machine before it runs:
// "exec format error" from inside a container says nothing about why.
// An image not here yet is left to docker run, which pulls the one for
// this machine or says there is none.
func (d Docker) checkArch(ctx context.Context, image string) error {
	out, err := exec.CommandContext(ctx, d.bin(), "image", "inspect", "-f", "{{.Architecture}}", image).Output()
	if err != nil {
		return nil
	}
	got := strings.TrimSpace(string(out))
	if got != "" && got != goruntime.GOARCH {
		return fmt.Errorf("the image %s is built for %s and this machine is %s: use a multi-arch base image, and copy binaries from an image (COPY --from=<image>) rather than downloading one for a fixed architecture", image, got, goruntime.GOARCH)
	}
	return nil
}

// lastLines are the last n non-empty lines of a file, joined.
func lastLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Checker is an instance that can be asked whether it is well from
// inside: a container, with docker exec.
type Checker interface {
	Check(ctx context.Context, argv []string) error
}

// Check runs argv inside the running container and passes when it exits
// 0: how a service no port on the machine reaches (a database on the
// site's network only) says it is ready.
func (c *dockerInstance) Check(ctx context.Context, argv []string) error {
	out, err := exec.CommandContext(ctx, c.d.bin(), append([]string{"exec", c.name}, argv...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// ErrNoCheck says an instance cannot be checked from inside.
var ErrNoCheck = errors.New("this instance cannot be checked from inside")
