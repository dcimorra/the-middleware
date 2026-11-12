// Package pubports keeps what the house knows about its sites' public
// ports beyond their manifests: which of them the owner approved, which
// are taken, and what is open in the machine's firewall.
//
// The approval is the part that needs a place of its own. A manifest is
// a file in the site's folder, and the site's agent and a delegate's
// shell write there; services.<name>.ports opens a port of the machine
// to the internet and network: host puts a container on the machine's
// loopback, next to every other site's ports. Refusing those keys at the
// API (as limits or repositories are) stops an account, not a file edit.
// So the engine starts such a service only when what its manifest asks is
// in this book, and only the owner writes in it: by saving the manifest
// themselves, by approving what someone else wrote, or by moving the site
// here.
package pubports

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"syscall"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Dir is the book's folder under data/. One file per site, named after
// it, so a delete and a rename take it along with the rest of what the
// house keeps by a site's name (sites.named).
const Dir = "ports"

// Book is the approvals under root/data/ports.
type Book struct {
	Root string
	mu   sync.Mutex
	// read is what was read of each site's file: the engine asks on every
	// pass, and the panel's every look at a site, under the engine's
	// lock. Release drops a site's, for whoever moves or removes the
	// file from outside (a rename, a delete).
	read map[string][]string
}

type record struct {
	// Approved are the lines of manifest.PublicGrant the owner approved.
	Approved []string `json:"approved"`
}

func (b *Book) path(domain string) string {
	return filepath.Join(b.Root, "data", Dir, domain+".json")
}

// Approved are the lines approved for domain.
func (b *Book) Approved(domain string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if have, ok := b.read[domain]; ok {
		return have
	}
	var r record
	_ = fsx.ReadJSON(b.path(domain), &r)
	if b.read == nil {
		b.read = map[string][]string{}
	}
	b.read[domain] = r.Approved
	return r.Approved
}

// Release forgets what was read of domain's file, which is about to be
// moved or removed.
func (b *Book) Release(domain string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.read, domain)
}

// Pending are the lines m asks that the owner has not approved, in order.
// Empty is a manifest the engine may act on.
func (b *Book) Pending(domain string, m *manifest.Manifest) []string {
	grant := m.PublicGrant()
	if len(grant) == 0 {
		return nil
	}
	have := b.Approved(domain)
	var out []string
	for _, line := range grant {
		if !slices.Contains(have, line) {
			out = append(out, line)
		}
	}
	return out
}

// Approve writes that the owner approves exactly what m asks now: what it
// no longer asks stops being approved, so a port taken out and written
// back by someone else waits again. It returns the lines that were new.
func (b *Book) Approve(domain string, m *manifest.Manifest) ([]string, error) {
	grant := m.PublicGrant()
	added := b.Pending(domain, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.read, domain)
	if len(grant) == 0 {
		if err := os.Remove(b.path(domain)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(b.path(domain)), 0o755); err != nil {
		return nil, err
	}
	return added, fsx.WriteJSON(b.path(domain), record{Approved: grant}, 0o644)
}

// Revoke forgets every approval of domain: its services with public
// ports stop at the engine's next pass.
func (b *Book) Revoke(domain string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.read, domain)
	if err := os.Remove(b.path(domain)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Conflicts are the reasons m's public ports cannot be domain's on this
// node: one the house holds (reserved, by port, with what holds it), or
// one another site declares. others are the other sites' manifests by
// name; hide keeps their names out of the answer, for a caller who sees
// one site and not the house.
func Conflicts(domain string, m *manifest.Manifest, others map[string]*manifest.Manifest, reserved []manifest.HeldPort, hide bool) []string {
	var out []string
	names := make([]string, 0, len(others))
	for n := range others {
		if n != domain {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, sp := range m.PublicPorts() {
		field := "services." + sp.Service + ".ports: " + sp.PublicPort.String()
		if h, ok := manifest.Holds(reserved, sp.PublicPort); ok {
			out = append(out, fmt.Sprintf("%s: %s is %s", field, h, h.What))
		}
		for _, n := range names {
			for _, op := range others[n].PublicPorts() {
				if !sp.Overlaps(op.PublicPort) {
					continue
				}
				if hide {
					out = append(out, field+": another site of this house has it")
				} else {
					out = append(out, fmt.Sprintf("%s: %s has %s (services.%s)", field, n, op.PublicPort, op.Service))
				}
			}
		}
	}
	return out
}

// Bound says which of ports something on this machine listens on already,
// by trying to listen there: the first and the last of a range, which is
// what a check before a move can afford (a relay binds its range one
// port at a time, as calls come).
func Bound(ports []manifest.PublicPort) []string {
	var out []string
	for _, p := range ports {
		for _, port := range edge(p) {
			if !free(p.Proto, port) {
				out = append(out, strconv.Itoa(port)+"/"+p.Proto)
			}
		}
	}
	return out
}

func edge(p manifest.PublicPort) []int {
	if p.From == p.To {
		return []int{p.From}
	}
	return []int{p.From, p.To}
}

func free(proto string, port int) bool {
	addr := ":" + strconv.Itoa(port)
	var err error
	if proto == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			l.Close()
		}
	}
	// Only "in use" says taken: a port below 1024 is refused to the
	// supervisor, which is not root, whoever holds it or nobody.
	return !errors.Is(err, syscall.EADDRINUSE)
}

// Parse reads the entries of a list as the manifest writes them.
func Parse(list []string) ([]manifest.PublicPort, error) {
	out := make([]manifest.PublicPort, 0, len(list))
	for _, raw := range list {
		p, err := manifest.ParsePublicPort(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
