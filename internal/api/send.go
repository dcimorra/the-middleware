package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
)

// A file from one node to a site on another. The day a site moved
// without its database, the copy of it was made by the house's agent on
// the node it had left, and had no way to the site that needed it but
// the operator carrying it by hand between two machines: each node's
// agent sees its own disk, and a site's agent only its folder and HOME.
// Now the operator, or the house's agent for them, sends a file through
// the mesh, and it lands in the site's HOME, under inbox/: where the
// site's agent can read it and nothing serves it.

// InboxDir is the folder under a site's HOME where what was sent to it
// from another node lands.
const InboxDir = "inbox"

var inboxNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)

// sent is what a node answers for a file it took.
type sent struct {
	Site   string `json:"site"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Node   string `json:"node,omitempty"`
}

// wholeAccess says the caller is the operator or another node, not a
// token that opens part of the house.
func wholeAccess(r *http.Request) bool {
	return len(auth.FromContext(r.Context()).Scopes) == 0
}

// registerInbox mounts the receiving end; it needs no mesh.
func (s *Server) registerInbox() {
	s.Handle("PUT", Prefix+"/sites/{domain}/inbox/{name}", "The operator or another node only: a file for the site, the raw body, kept in its HOME under inbox/ (never served, read by its agent and its processes as ~/inbox/<name>); a name that is there is replaced. Answers its size and SHA-256 as written", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if !wholeAccess(r) {
			return nil, Errorf(http.StatusForbidden, "only the operator or another node leaves a file in a site's inbox")
		}
		domain, name := r.PathValue("domain"), r.PathValue("name")
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a site's name", domain)
		}
		if !inboxNameRe.MatchString(name) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a file name for an inbox (letters, digits, . _ + -, no folders)", name)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Get(domain); err != nil {
			return nil, Errorf(http.StatusNotFound, "%s is not a site on this node", domain)
		}
		dir := filepath.Join(s.opts.Root, "data", "homes", domain, InboxDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// Beside where it will be, so the rename is one, and under a name
		// nobody reads: a file half arrived is not in the inbox.
		tmp, err := os.CreateTemp(dir, "."+name+".*.part")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, hash), r.Body)
		if err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("the file did not arrive whole (%d bytes of it did): %w", n, err)
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
			return nil, err
		}
		out := sent{Site: domain, Name: name, Path: "~/" + InboxDir + "/" + name, Bytes: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
		from := "the operator"
		if node, ok := s.fromNode(auth.FromContext(r.Context())); ok {
			from = "the node " + node
		}
		s.emitMove("site.inbox", domain, "", fmt.Sprintf("%s left %s in its inbox (%d bytes): %s", from, name, n, out.Path), map[string]any{"name": name, "bytes": n, "sha256": out.SHA256})
		return out, nil
	})
}

// memberNamed is the node of the network called so, by its id or its
// name.
func memberNamed(m *nodes.Mesh, who string) (nodes.Member, bool) {
	if x, ok := m.Member(who); ok {
		return x, true
	}
	var found []nodes.Member
	for _, x := range m.Members() {
		if strings.EqualFold(x.Name, who) {
			found = append(found, x)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return nodes.Member{}, false
}

// registerSend mounts the sending end: it needs the mesh.
func (s *Server) registerSend(m *nodes.Mesh) {
	s.Handle("POST", Prefix+"/nodes/{id}/send", "The operator only: send a file of this machine (body: path, site, and name if it should arrive under another) to a site on another node of the network (its id or its name), into that site's inbox (~/inbox/<name> there). Answers what the node wrote, checked against what was read here", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 || c.Via == auth.ViaNode {
			return nil, Errorf(http.StatusForbidden, "only the operator sends a file of this machine to another node")
		}
		var body struct {
			Path string `json:"path"`
			Site string `json:"site"`
			Name string `json:"name"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		x, ok := memberNamed(m, r.PathValue("id"))
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no node of the network is called %q (middleware nodes lists them)", r.PathValue("id"))
		}
		site := strings.ToLower(strings.TrimSpace(body.Site))
		if !manifest.ValidName(site) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a site's name", body.Site)
		}
		if !filepath.IsAbs(body.Path) {
			return nil, Errorf(http.StatusBadRequest, "path: the whole path of the file on this machine")
		}
		name := body.Name
		if name == "" {
			name = filepath.Base(body.Path)
		}
		if !inboxNameRe.MatchString(name) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a file name for an inbox (letters, digits, . _ + -): give another with name", name)
		}
		f, err := os.Open(body.Path)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		defer f.Close()
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
			return nil, Errorf(http.StatusBadRequest, "%s is not a file (a folder goes as a tar of it)", body.Path)
		}
		// A file of gigabytes takes what it takes; a node that stops
		// answering is the connection's to notice.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)
		defer cancel()
		hash := sha256.New()
		var out sent
		if err := m.Stream(ctx, x, "PUT", "/sites/"+site+"/inbox/"+name, "application/octet-stream", io.TeeReader(f, hash), &out); err != nil {
			return nil, Errorf(http.StatusBadGateway, "%s did not take it: %v", x.Name, err)
		}
		if here := hex.EncodeToString(hash.Sum(nil)); out.SHA256 != here {
			return nil, Errorf(http.StatusBadGateway, "%s wrote something else than what was read here (sha256 %s there, %s here): send it again", x.Name, out.SHA256, here)
		}
		out.Node = x.Name
		s.emitMove("node.sent", site, "", fmt.Sprintf("%s sent to %s on %s (%d bytes)", body.Path, site, x.Name, out.Bytes), map[string]any{"node": x.Name, "name": name, "sha256": out.SHA256})
		return out, nil
	})
}
