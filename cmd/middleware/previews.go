package main

import (
	"context"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/modules/github"
	"github.com/dcimorra/the-middleware/internal/publish"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// wirePreviews lets a push to a branch make, deploy or remove the branch's
// preview of every site that has previews on (manifest previews), and
// sweeps the stale ones every hour. The publisher may be nil (no inbound
// hook): the preview is made all the same and its name is not published.
func wirePreviews(a *app.App, gh *github.Module, mgr *sites.Manager, pub *publish.Publisher) {
	if gh == nil || mgr == nil {
		return
	}
	emitErr := func(subject string, err error) {
		a.Bus.Emit(events.Event{Type: "preview.failed", Subject: subject, Actor: "github", Level: "error", Message: err.Error()})
	}
	unpublish := func(name string) {
		if pub == nil {
			return
		}
		if _, ok := pub.Published(name); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			pub.Unpublish(ctx, name)
		}
	}
	gh.Previews = func(push github.Push, actor string) []string {
		branch, ok := strings.CutPrefix(push.Ref, "refs/heads/")
		if !ok {
			return nil
		}
		var out []string
		for _, sn := range a.Engine.Sites() {
			m := sn.Manifest
			if m == nil || m.Previews == nil || m.Staging != nil || m.Deploy == nil || m.Deploy.GitHub == nil {
				continue
			}
			g := m.Deploy.GitHub
			if !strings.EqualFold(strings.TrimSuffix(g.Repository, ".git"), strings.TrimSuffix(push.Repository.FullName, ".git")) ||
				branch == deploy.Branch(g) || !m.Previews.Wants(branch) {
				continue
			}
			domain := sn.Domain
			if push.Deleted {
				name, err := mgr.DropPreview(context.Background(), domain, branch)
				if err == nil {
					go unpublish(name)
					out = append(out, name)
				}
				continue
			}
			out = append(out, previewName(mgr, domain, branch))
			// Making one copies the site's files and databases, which
			// can take minutes: the hook answers GitHub now.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				name, created, err := mgr.Preview(ctx, domain, branch, actor)
				if err != nil {
					emitErr(domain, err)
					return
				}
				if created && pub != nil {
					if _, err := pub.Publish(ctx, name, inbound.Door("")); err != nil {
						a.Bus.Emit(events.Event{Type: "publish.failed", Subject: name, Actor: "github", Level: "error", Message: err.Error()})
					}
				}
			}()
		}
		return out
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			for _, name := range mgr.ExpirePreviews(context.Background(), time.Now()) {
				unpublish(name)
			}
		}
	}()
}

// previewName is the preview's name as it is or will be.
func previewName(mgr *sites.Manager, domain, branch string) string {
	for _, p := range mgr.Previews(domain) {
		if p.Branch == branch {
			return p.Name
		}
	}
	return manifest.PreviewName(domain, branch)
}
