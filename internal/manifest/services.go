package manifest

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/attack"
	"github.com/dcimorra/the-middleware/internal/security"
)

// Service is one of a site's processes when the site has several: a mock
// in Node, a Next.js app and a Drupal with its database, each answering
// its own names, built, health-checked, handed over and restarted on its
// own. Before services, such a project was one image with a router and a
// supervisor in bash inside it: a typo in one line of it kept every part
// from deploying, and while Drupal installed its name answered a raw 502
// from the router inside.
//
// What a service runs is a run section, the same one a site has, with the
// same rules. What it adds is the names it answers, the order it starts
// in, and the rules that would otherwise be the site's.
type Service struct {
	Run *Run `json:"run"`
	// Domains are the names this service answers: "host" or
	// "host/prefix". The longest prefix of a host wins, by whole
	// segments; the path reaches the service as it came. An apex brings
	// its www. with it, as a site's does.
	Domains []string `json:"domains,omitempty"`
	// Internal says the service answers no name, only the site's other
	// services. It is what no domains means; writing it is for whoever
	// reads the manifest, and writing it with domains is refused.
	Internal bool `json:"internal,omitempty"`
	// DependsOn are services that must be healthy before this one's
	// first start.
	DependsOn []string `json:"depends_on,omitempty"`
	// Optional keeps the site counted as up while this service is down.
	Optional bool `json:"optional,omitempty"`
	// Handover, empty, is true for a service with names and false for
	// one without: a database cannot run two generations on one volume,
	// so it is stopped and started again instead.
	Handover *bool  `json:"handover,omitempty"`
	Note     string `json:"note,omitempty"`
	// Ports are the ones the service answers on the machine's own
	// addresses, with no proxy of the house in between: "3478/udp",
	// "3478/tcp", "49160-49400/udp" (ports.go). The owner's to grant.
	Ports []string `json:"ports,omitempty"`
	// Network "host" runs the service's container on the machine's
	// network instead of the site's: what a relay that hands out hundreds
	// of UDP ports needs, since docker starts a proxy per published port.
	// Such a container sees the machine's loopback, every other site's
	// ports on it included, which is why it is the owner's to grant.
	Network string `json:"network,omitempty"`

	// The rules that are the site's when the service does not say:
	// access and security narrow the site's, the rest replace it.
	Disabled bool              `json:"disabled,omitempty"`
	Hold     *Hold             `json:"hold,omitempty"`
	Access   *Access           `json:"access,omitempty"`
	Security *security.Rules   `json:"security,omitempty"`
	WAF      *WAF              `json:"waf,omitempty"`
	Attack   *attack.Rules     `json:"attack,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Cache    *Cache            `json:"cache,omitempty"`
}

// HandsOver says whether a new generation of the service starts next to
// the old one (true) or after it stops (false).
func (s *Service) HandsOver() bool {
	if s.Run != nil && s.Run.Port != 0 {
		return false // a written port holds one generation
	}
	if s.Public() {
		return false // and so does a port on the machine
	}
	if s.Handover != nil {
		return *s.Handover
	}
	return len(s.Domains) > 0
}

// serviceNameRe is a service's name: it is a name in the site's network
// (http://db:3306) and part of a variable ($SERVICE_db_HOST, a dash as an
// underscore), so it is what both accept, and no underscore, or "a-b"
// and "a_b" would be one variable.
var serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ServiceNames are the services, sorted.
func (m *Manifest) ServiceNames() []string {
	out := make([]string, 0, len(m.Services))
	for n := range m.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ServiceVar is the variable a service's address is read from:
// SERVICE_<name>_HOST, SERVICE_<name>_PORT.
func ServiceVar(name, what string) string {
	return "SERVICE_" + strings.ReplaceAll(name, "-", "_") + "_" + what
}

// Route is one name, or a name and a path prefix, and the service that
// answers it.
type Route struct {
	Host    string `json:"host"`
	Prefix  string `json:"prefix,omitempty"`
	Service string `json:"service"`
}

// SplitDomain splits "host/prefix" into its host and its prefix ("" or
// "/prefix"), the host lower-cased.
func SplitDomain(d string) (host, prefix string) {
	host, rest, ok := strings.Cut(strings.TrimSpace(d), "/")
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ok {
		prefix = "/" + strings.TrimSuffix(rest, "/")
		if prefix == "/" {
			prefix = ""
		}
	}
	return host, prefix
}

// Routes is every route of the site's services, its apexes' www.
// included, sorted by host and then by the longest prefix first, which
// is the order they are matched in.
func (m *Manifest) Routes() []Route {
	var out []Route
	seen := map[string]bool{}
	www := m.WWW == nil || *m.WWW
	add := func(r Route) {
		k := r.Host + r.Prefix
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	for _, name := range m.ServiceNames() {
		for _, d := range m.Services[name].Domains {
			host, prefix := SplitDomain(d)
			add(Route{Host: host, Prefix: prefix, Service: name})
		}
	}
	// The www of an apex goes where the apex goes, written after so a www
	// written as a domain of its own wins.
	for _, r := range append([]Route(nil), out...) {
		if www && strings.Count(r.Host, ".") == 1 && !strings.HasPrefix(r.Host, "www.") {
			add(Route{Host: "www." + r.Host, Prefix: r.Prefix, Service: r.Service})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return len(out[i].Prefix) > len(out[j].Prefix)
	})
	return out
}

// ServiceFor is the service that answers host and urlPath, by the longest
// prefix by whole segments; false when none does (a host the site does
// not serve, or a path no prefix of it covers).
func (m *Manifest) ServiceFor(host, urlPath string) (string, bool) {
	if m == nil || len(m.Services) == 0 {
		return "", false
	}
	p := path.Clean("/" + urlPath)
	for _, r := range m.Routes() {
		if r.Host != host {
			continue
		}
		if r.Prefix == "" || p == r.Prefix || strings.HasPrefix(p, r.Prefix+"/") {
			return r.Service, true
		}
	}
	return "", false
}

// serviceRefRe finds $SERVICE_<name>_HOST and _PORT, braced or not.
var serviceRefRe = regexp.MustCompile(`\$\{?SERVICE_([A-Za-z0-9_]+?)_(HOST|PORT)\}?`)

// ServiceRefs are the services a run section names through
// $SERVICE_<name>_HOST or _PORT, in its command and its env, by the
// variable's spelling (a dash as an underscore).
func ServiceRefs(r *Run) []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	scan := func(s string) {
		for _, m := range serviceRefRe.FindAllStringSubmatch(s, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	for _, a := range r.Command {
		scan(a)
	}
	for _, v := range r.Env {
		scan(v)
	}
	sort.Strings(out)
	return out
}

// pinnedImage says whether an image names the version it runs: a tag
// other than latest, or a digest. An image that moves underneath the
// house changes what a site runs without a manifest that says so, and
// two generations of a handover may run two different images.
func pinnedImage(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return true
	}
	last := image[strings.LastIndex(image, "/")+1:]
	_, tag, ok := strings.Cut(last, ":")
	return ok && tag != "" && tag != "latest"
}

// validServices checks the services section and how it sits with the
// rest of the manifest.
func (m *Manifest) validServices() error {
	if len(m.Services) == 0 {
		return nil
	}
	switch {
	case m.Run != nil:
		return fmt.Errorf("services: a site has run or services, not both (a run is one service: move it into services)")
	case m.Upstream != "":
		return fmt.Errorf("services: a site with services has no upstream")
	case m.Static != nil:
		return fmt.Errorf("services: a site with services has no static section")
	case len(m.Aliases) > 0:
		return fmt.Errorf("services: a site with services has no aliases; each service names its own domains")
	}
	owner := map[string]string{}
	byVar := map[string]string{}
	var public []ServicePort
	for _, name := range m.ServiceNames() {
		sv := m.Services[name]
		field := "services." + name
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("services: %q is not a service name (a-z, 0-9 and -, starting with a letter, 32 at most)", name)
		}
		if _, clash := m.Workers[name]; clash {
			return fmt.Errorf("%s: a worker has the same name", field)
		}
		if sv == nil || sv.Run == nil {
			return fmt.Errorf("%s.run: missing", field)
		}
		if err := validRun(field+".run", sv.Run, true); err != nil {
			return err
		}
		if d := sv.Run.Docker; d != nil && d.Image != "" && d.Build == "" && !pinnedImage(d.Image) {
			return fmt.Errorf("%s.run.docker.image: %q names no version; write its tag (mariadb:11.4) or digest, never latest", field, d.Image)
		}
		if sv.Internal && len(sv.Domains) > 0 {
			return fmt.Errorf("%s: internal and domains say opposite things", field)
		}
		if sv.Handover != nil && *sv.Handover && sv.Run.Port != 0 {
			return fmt.Errorf("%s: a written run.port holds one generation, it cannot hand over", field)
		}
		if err := m.validPublic(name, sv, &public); err != nil {
			return err
		}
		byVar[strings.ReplaceAll(name, "-", "_")] = name
		for _, d := range sv.Domains {
			host, prefix := SplitDomain(d)
			if !ValidDomain(host) {
				return fmt.Errorf("%s.domains: %q is not a domain", field, d)
			}
			if prefix != "" && (path.Clean(prefix) != prefix || strings.ContainsAny(prefix, "?#*")) {
				return fmt.Errorf("%s.domains: %q: the path after the name is a clean prefix, no wildcards", field, d)
			}
			k := host + prefix
			if other, dup := owner[k]; dup {
				if other == name {
					return fmt.Errorf("%s.domains: %q twice", field, d)
				}
				return fmt.Errorf("%s.domains: %q is already services.%s's; a name answers one service", field, d, other)
			}
			owner[k] = name
		}
		if err := validHold(field+".hold", sv.Hold); err != nil {
			return err
		}
		if err := validAccess(field+".access", sv.Access); err != nil {
			return err
		}
		if err := m.narrowAccess(field, sv.Access); err != nil {
			return err
		}
		if err := validLive(field+".", sv.Cache, sv.Attack, sv.Security, sv.WAF); err != nil {
			return err
		}
	}
	for _, name := range m.ServiceNames() {
		sv := m.Services[name]
		field := "services." + name
		for _, dep := range sv.DependsOn {
			if dep == name {
				return fmt.Errorf("%s.depends_on: a service cannot wait for itself", field)
			}
			if _, ok := m.Services[dep]; !ok {
				return fmt.Errorf("%s.depends_on: %q is not one of the services", field, dep)
			}
		}
		for _, ref := range ServiceRefs(sv.Run) {
			target, ok := byVar[ref]
			if !ok {
				return fmt.Errorf("%s.run: $SERVICE_%s_… names no service", field, ref)
			}
			// Neither way: a container on the machine's network is not on
			// the site's, where the names are.
			if sv.HostNetwork() || m.Services[target].HostNetwork() {
				return fmt.Errorf("%s.run: names services.%s, and one of the two is on the host's network (network: host), where the site's names do not resolve; reach it by the site's public name and its port", field, target)
			}
			// A process reaches a service on its port on the machine,
			// which changes with every generation of one that hands over:
			// it would keep the port it was born with. A container
			// reaches it by name on a port that does not change.
			if runtimeOf(sv.Run.Runtime) == "process" && m.Services[target].HandsOver() {
				return fmt.Errorf("%s.run: names services.%s, which hands over to a new port every generation; reach it from a container, or give it handover: false", field, target)
			}
			// A container sees its own loopback, not the machine's, and a
			// process listens on the machine's.
			if runtimeOf(sv.Run.Runtime) == "docker" && runtimeOf(m.Services[target].Run.Runtime) == "process" {
				return fmt.Errorf("%s.run: names services.%s, a process on the machine, which a container cannot reach; run it in a container too", field, target)
			}
		}
		if !m.Published(name) && sv.Run.Health != nil && sv.Run.Health.Path != "" {
			return fmt.Errorf("%s.run.health.path: the service is only on the site's network, out of the house's reach; check it with health.command, run inside it", field)
		}
	}
	if cycle := m.dependsCycle(); cycle != "" {
		return fmt.Errorf("services.depends_on: a cycle: %s", cycle)
	}
	return nil
}

func runtimeOf(r string) string {
	if r == "" {
		return "process"
	}
	return r
}

// narrowAccess refuses a service's access that would widen the site's,
// or that could only be read as narrowing it by intersecting lists: an
// allow or a password at both levels, a zone at both levels. What stays
// (a deny, a zone of its own, an allow where the site has none) only
// ever closes more.
func (m *Manifest) narrowAccess(field string, sa *Access) error {
	if sa == nil || m.Access == nil {
		return nil
	}
	if len(sa.Allow) > 0 && len(m.Access.Allow) > 0 {
		return fmt.Errorf("%s.access.allow: the site has an allow list already; write it at one level", field)
	}
	if sa.Password != "" && m.Access.Password != "" {
		return fmt.Errorf("%s.access.password: the site has a password already; write it at one level", field)
	}
	for prefix := range sa.Routes {
		if _, both := m.Access.Routes[prefix]; both {
			return fmt.Errorf("%s.access.routes: %q is a zone of the site's already; write it at one level", field, prefix)
		}
	}
	return nil
}

// dependsCycle is a cycle in depends_on spelled out ("a → b → a"), or "".
func (m *Manifest) dependsCycle() string {
	const (
		unseen = iota
		open
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(n string) string
	visit = func(n string) string {
		switch state[n] {
		case open:
			i := len(stack) - 1
			for i > 0 && stack[i] != n {
				i--
			}
			return strings.Join(append(append([]string(nil), stack[i:]...), n), " → ")
		case done:
			return ""
		}
		state[n] = open
		stack = append(stack, n)
		deps := append([]string(nil), m.Services[n].DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if c := visit(d); c != "" {
				return c
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
		return ""
	}
	for _, n := range m.ServiceNames() {
		if c := visit(n); c != "" {
			return c
		}
	}
	return ""
}

// checkServicesName refuses a site whose own name no service answers:
// the folder's name would fall to the house's page for a name nobody
// serves, which is a site that looks down for a reason nobody wrote.
func (m *Manifest) checkServicesName(name string) error {
	if len(m.Services) == 0 || !ValidDomain(name) {
		return nil
	}
	for _, r := range m.Routes() {
		if r.Host == name && r.Prefix == "" {
			return nil
		}
	}
	for _, r := range m.Routes() {
		if r.Host == name {
			return fmt.Errorf("services: %s has prefixes only; one service must answer the rest of it (a domain with no path)", name)
		}
	}
	return fmt.Errorf("services: no service answers %s, the site's own name; add it to one service's domains", name)
}

// ForService is the manifest a request for one service is judged and
// answered under: the site's, with what the service says in place of
// waf, attack, headers, cache and hold. Access and security are not
// replaced here: they narrow, so both levels are evaluated, the site's
// from this manifest and the service's from Services[name] (a delegate
// may write a service's access, and a service's allow list that replaced
// the site's was a way past it).
//
// The copy is shallow and made per call; the hooks key what they compile
// on the pointers inside it, which are the loaded manifest's.
func (m *Manifest) ForService(name string) *Manifest {
	sv := m.Services[name]
	if sv == nil {
		return m
	}
	out := *m
	if sv.WAF != nil {
		out.WAF = sv.WAF
	}
	if sv.Attack != nil {
		out.Attack = sv.Attack
	}
	if sv.Cache != nil {
		out.Cache = sv.Cache
	}
	if sv.Headers != nil {
		out.Headers = sv.Headers
	}
	if sv.Hold != nil && sv.Hold.Enabled && (m.Hold == nil || !m.Hold.Enabled) {
		out.Hold = sv.Hold
	}
	return &out
}
