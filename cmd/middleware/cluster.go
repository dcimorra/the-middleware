package main

import (
	"context"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/cluster"
)

// theCluster keeps this node's sites' replicas on the others and the
// others' here (internal/cluster). Built with the hooks; the mesh, made
// later by the wire hook, is plugged in there.
var theCluster *cluster.Manager

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "cluster", Order: orderCluster, Build: func(a *app.App) app.Hook {
		theCluster = cluster.New(cluster.Options{Root: a.Root, Sites: a.Engine, Bus: a.Bus})
		return clusterHook{theCluster}
	}})
}

type clusterHook struct{ m *cluster.Manager }

func (h clusterHook) Wrap(next http.Handler) http.Handler { return h.m.Wrap(next) }

func (h clusterHook) Run(ctx context.Context, a *app.App) error {
	go h.m.Run(ctx)
	return nil
}
