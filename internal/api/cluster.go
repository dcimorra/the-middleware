package api

import (
	"errors"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/cluster"
	"github.com/dcimorra/the-middleware/internal/nodes"
)

// RegisterCluster mounts the cluster's routes: what a primary calls on the
// nodes that keep its replicas (the round, a copy, its commit, its
// removal), which only another node of the network may call, and what
// the operator reads of a site's cluster. Turning a cluster on or off is
// the manifest's (cluster), written as any part of it is.
func (s *Server) RegisterCluster(c *cluster.Manager) {
	if c == nil {
		return
	}
	c.SetInstall(installDeps)
	node := func(r *http.Request) (nodes.Member, error) {
		x, ok := nodes.MemberFrom(r.Context())
		if !ok || auth.FromContext(r.Context()).Via != auth.ViaNode {
			return nodes.Member{}, Errorf(http.StatusForbidden, "only another node of the network calls this")
		}
		return x, nil
	}
	bad := func(err error) error {
		switch {
		case errors.Is(err, cluster.ErrTaken), errors.Is(err, cluster.ErrBusy):
			return Errorf(http.StatusConflict, "%v", err)
		case errors.Is(err, cluster.ErrBadName), errors.Is(err, cluster.ErrNoIncoming):
			return Errorf(http.StatusBadRequest, "%v", err)
		case errors.Is(err, cluster.ErrNotReplica):
			return Errorf(http.StatusNotFound, "%v", err)
		}
		return Errorf(http.StatusUnprocessableEntity, "%v", err)
	}
	s.Handle("POST", Prefix+"/cluster/status", "A primary's round (nodes only; body: expect, its sites kept here): this node's load and that primary's replicas here; the ones it no longer names are removed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		x, err := node(r)
		if err != nil {
			return nil, err
		}
		var body cluster.StatusRequest
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		return c.Status(x, body), nil
	})
	s.Handle("PUT", Prefix+"/cluster/replicas/{domain}", "Receive a copy of a primary's site (nodes only): its folder as a .tar.gz, kept apart until commit", func(w http.ResponseWriter, r *http.Request) (any, error) {
		x, err := node(r)
		if err != nil {
			return nil, err
		}
		defer s.receiving()()
		rep, err := c.Receive(r.Context(), x, r.PathValue("domain"), r.Body)
		if err != nil {
			return nil, bad(err)
		}
		return rep, nil
	})
	s.Handle("POST", Prefix+"/cluster/replicas/{domain}/commit", "Put a received copy in place (nodes only; body: manifest base64, revision, primary, dirs): its dependencies installed for this machine, then swapped for the copy that serves; answers at once", func(w http.ResponseWriter, r *http.Request) (any, error) {
		x, err := node(r)
		if err != nil {
			return nil, err
		}
		var body cluster.CommitRequest
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := c.Commit(x, r.PathValue("domain"), body); err != nil {
			return nil, bad(err)
		}
		return map[string]any{"started": true}, nil
	})
	s.Handle("DELETE", Prefix+"/cluster/replicas/{domain}", "Remove a replica kept here: its primary, or the operator", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var from *nodes.Member
		if x, err := node(r); err == nil {
			from = &x
		} else if cl := auth.FromContext(r.Context()); cl.Delegate() || len(cl.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator removes a replica")
		}
		if err := c.Remove(from, r.PathValue("domain")); err != nil {
			return nil, bad(err)
		}
		return OK, nil
	})
	s.Handle("GET", Prefix+"/cluster", "This node's part in the clusters: its load, and the replicas it keeps for other nodes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if cl := auth.FromContext(r.Context()); cl.Delegate() || len(cl.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator sees the cluster")
		}
		return map[string]any{"load": c.LocalLoad(), "kept": c.Kept()}, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/cluster", "The site's cluster: whether it can have replicas (and why not), where they are, their state and load, and whether this node is sharing its requests now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if cl := auth.FromContext(r.Context()); cl.Delegate() || len(cl.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator sees the cluster")
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		v, ok := c.View(domain)
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no site %s here", domain)
		}
		return v, nil
	})
}
