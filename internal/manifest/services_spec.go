package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// DefaultServicePort is the port a service's container listens on inside
// when its docker section names none. A service's port on the machine
// changes with every generation of a handover, and the other containers
// of the site reach it by name: a port inside that moved with it would
// be a port they could never write down.
const DefaultServicePort = 8080

// ServiceAddr says where a service is reached from another one: from a
// container, by its name on the site's network and its port inside; from
// a process on the machine, on loopback and its port there. ok is false
// while the target has no port yet.
type ServiceAddr func(target string, fromContainer bool) (host string, port int, ok bool)

// DockerNetwork is a site's own network, its services' containers joined
// to it: what they reach each other by name on, and nobody else's.
func DockerNetwork(site string) string {
	return "mw-" + strings.NewReplacer(".", "-", "/", "-").Replace(site) + "-net"
}

// Published says whether the service's port is published on the
// machine's loopback. A container that answers no name and that no
// process of the site reaches is not: the machine's loopback is every
// site's, and the database of one was there for any other to dial.
func (m *Manifest) Published(name string) bool {
	sv := m.Services[name]
	if sv.HostNetwork() {
		// There is no port to publish: the container binds the machine's
		// own, and the house reaches it by health.command.
		return false
	}
	if sv == nil || sv.Run == nil || runtimeOf(sv.Run.Runtime) != "docker" || len(sv.Domains) > 0 {
		return true
	}
	varName := strings.ReplaceAll(name, "-", "_")
	for _, other := range m.ServiceNames() {
		o := m.Services[other]
		if other == name || runtimeOf(o.Run.Runtime) == "docker" {
			continue
		}
		if slices.Contains(o.DependsOn, name) || slices.Contains(ServiceRefs(o.Run), varName) {
			return true
		}
	}
	return false
}

// ServiceSpec is what a service's instance is born from: its run section
// resolved like a site's, $SERVICE_<name>_HOST and _PORT expanded through
// addr, in the site's network when it runs in a container.
func ServiceSpec(site, dir, name string, m *Manifest, addr ServiceAddr) (*Spec, error) {
	return ServiceSpecOn(site, dir, name, m, addr, "")
}

// ErrNoPublicIP says a service that needs the node's public address
// cannot be resolved yet: the house has not found it.
var ErrNoPublicIP = errors.New("waiting for this node's public address ($" + PublicIPVar + "): the house has not found it yet (Settings → Publishing sets it by hand)")

// ServiceSpecOn is ServiceSpec on a node whose public address is
// publicIP: $MIDDLEWARE_PUBLIC_IP in the command and the env is that, and
// a service with public ports has it in its environment too. Empty, the
// variable is left as written, which is for whoever only reads the spec
// (a one-off command, a check); the engine, which starts it, refuses
// with ErrNoPublicIP instead (Spec.NeedsPublicIP).
func ServiceSpecOn(site, dir, name string, m *Manifest, addr ServiceAddr, publicIP string) (*Spec, error) {
	sv := m.Services[name]
	if sv == nil || sv.Run == nil {
		return nil, fmt.Errorf("%s: no service %q", site, name)
	}
	docker := runtimeOf(sv.Run.Runtime) == "docker"
	extra := map[string]string{}
	byVar := map[string]string{}
	for _, n := range m.ServiceNames() {
		byVar[strings.ReplaceAll(n, "-", "_")] = n
	}
	for _, ref := range ServiceRefs(sv.Run) {
		target := byVar[ref]
		host, port, ok := addr(target, docker)
		if !ok {
			return nil, fmt.Errorf("services.%s: waiting for services.%s to have a port", name, target)
		}
		extra[ServiceVar(ref, "HOST")] = host
		extra[ServiceVar(ref, "PORT")] = strconv.Itoa(port)
	}
	if publicIP != "" {
		extra[PublicIPVar] = publicIP
	}
	spec, err := resolveRun(site, dir, m.Node, sv.Run, extra)
	if err != nil {
		return nil, err
	}
	spec.Domain = site + "/" + name
	spec.Service = name
	spec.Env["MIDDLEWARE_SERVICE"] = name
	spec.PublicPorts = sv.PublicPorts()
	spec.HostNetwork = sv.HostNetwork()
	if sv.Public() || usesPublicIP(sv.Run) {
		spec.NeedsPublicIP = publicIP == ""
		if sv.Public() && publicIP != "" {
			// In the environment too, for a command that is a shell line
			// reading its own variables (a secret from the site's .env
			// next to the address).
			spec.Env[PublicIPVar] = publicIP
		}
	}
	if docker && spec.HostNetwork {
		// No network of the site's and no port of its own inside: the
		// container is on the machine's.
		spec.Unpublished = true
	} else if docker {
		spec.Network = DockerNetwork(site)
		spec.ContainerPort = DefaultServicePort
		if sv.Run.Docker.Port != 0 {
			spec.ContainerPort = sv.Run.Docker.Port
		}
		spec.Unpublished = !m.Published(name)
	}
	if docker && sv.Run.Docker.Build != "" {
		spec.Fingerprint = Fingerprint(dir, watched(sv.Run.Docker))
	}
	spec.Rehash()
	return spec, nil
}

// watched are the paths a built service's revision reads: docker.watch,
// else the Dockerfile's folder, or the Dockerfile alone when it sits at
// the root (watching the whole site would rebuild on every upload it
// writes).
func watched(d *Docker) []string {
	if len(d.Watch) > 0 {
		return d.Watch
	}
	if dir := filepath.Dir(d.Build); dir != "." && dir != "" {
		return []string{dir}
	}
	return []string{d.Build}
}

// fingerprintLimit bounds the files a fingerprint walks: it runs every
// tick, and a watch that names a tree this large names the wrong tree.
const fingerprintLimit = 20000

// Fingerprint is what paths under dir look like now: names, sizes and
// times, not contents, so it is cheap enough for every tick. What a
// build makes and never reads back (.git, node_modules, .next) is left
// out.
func Fingerprint(dir string, paths []string) string {
	h := sha256.New()
	n := 0
	for _, p := range paths {
		root := filepath.Join(dir, p)
		_ = filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", ".next":
					return filepath.SkipDir
				}
				return nil
			}
			if n++; n > fingerprintLimit {
				return filepath.SkipAll
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(dir, full)
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00", rel, info.Size(), info.ModTime().UnixNano())
			return nil
		})
		if _, err := os.Stat(root); err != nil {
			fmt.Fprintf(h, "%s\x00missing\x00", p)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
