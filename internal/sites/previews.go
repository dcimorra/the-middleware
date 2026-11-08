package sites

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Preview is the test copy of domain that deploys branch, made on the
// first push to the branch: a copy like any other (files, databases,
// password; CreateStaging), whose manifest follows the branch instead of
// production's. A push to a branch that already has one only deploys
// it. created says the copy is new, so the caller publishes its name.
func (m *Manager) Preview(ctx context.Context, domain, branch, actor string) (name string, created bool, err error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return "", false, err
	}
	prod, err := manifest.Load(dir)
	if err != nil {
		return "", false, fmt.Errorf("manifest: %w", err)
	}
	if !prod.Previews.Wants(branch) || prod.Deploy == nil || prod.Deploy.GitHub == nil || branch == deploy.Branch(prod.Deploy.GitHub) {
		return "", false, fmt.Errorf("%s: %s does not get a preview", domain, branch)
	}
	if name, ok := m.previewOf(domain, branch); ok {
		return name, false, m.deployPreview(name, actor)
	}
	name = manifest.PreviewName(domain, branch)
	if _, err := m.CreateStaging(ctx, domain, name); err != nil {
		return "", false, err
	}
	pdir := m.Dir(name)
	mf, err := manifest.Load(pdir)
	if err != nil {
		return name, true, err
	}
	g := *mf.Deploy.GitHub
	g.Branch = branch
	mf.Deploy.GitHub = &g
	mf.Staging.Branch = branch
	if mf.Title != "" {
		mf.Title = strings.TrimSuffix(mf.Title, " (staging)") + " (" + branch + ")"
	}
	if mf.Access != nil && strings.HasPrefix(mf.Access.Message, "Test copy of ") {
		mf.Access.Message = "Preview of " + branch
	}
	if mf.Run != nil && mf.Run.Env != nil {
		// The code can tell which branch it is, besides that it is a copy.
		mf.Run.Env["MIDDLEWARE_PREVIEW"] = branch
	}
	if err := manifest.Save(pdir, mf); err != nil {
		return name, true, err
	}
	m.emit("preview.created", name, "", "preview of "+branch, map[string]any{"of": domain, "branch": branch})
	return name, true, m.deployPreview(name, actor)
}

func (m *Manager) deployPreview(name, actor string) error {
	if m.Deploys == nil {
		return errors.New("no deployment runner")
	}
	dir := m.Dir(name)
	mf, err := manifest.Load(dir)
	if err != nil {
		return err
	}
	m.Deploys.Trigger(name, dir, mf, actor)
	return nil
}

// previewOf finds the preview of domain for branch, by its mark rather
// than its name: a renamed preview is still the branch's.
func (m *Manager) previewOf(domain, branch string) (string, bool) {
	for _, p := range m.Previews(domain) {
		if p.Branch == branch {
			return p.Name, true
		}
	}
	return "", false
}

// PreviewInfo is one preview of a site.
type PreviewInfo struct {
	Name   string    `json:"name"`
	Branch string    `json:"branch"`
	Pushed time.Time `json:"pushed"`
}

// Previews lists the previews of domain.
func (m *Manager) Previews(domain string) []PreviewInfo {
	entries, _ := os.ReadDir(filepath.Join(m.Root, "sites"))
	var out []PreviewInfo
	for _, e := range entries {
		dir := filepath.Join(m.Root, "sites", e.Name())
		mf, err := manifest.Load(dir)
		if err != nil || mf.Staging == nil || mf.Staging.Branch == "" || mf.Staging.Of != domain {
			continue
		}
		out = append(out, PreviewInfo{Name: e.Name(), Branch: mf.Staging.Branch, Pushed: m.lastPush(e.Name(), dir)})
	}
	return out
}

// lastPush is when the preview last deployed, or was made.
func (m *Manager) lastPush(name, dir string) time.Time {
	if m.Deploys != nil {
		if st, err := m.Deploys.Last(name); err == nil && !st.Started.IsZero() {
			return st.Started
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "site.json")); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// DropPreview removes the preview of domain for branch (the branch was
// deleted). Its name is returned so the caller unpublishes it.
func (m *Manager) DropPreview(ctx context.Context, domain, branch string) (string, error) {
	name, ok := m.previewOf(domain, branch)
	if !ok {
		return "", os.ErrNotExist
	}
	if _, err := m.Delete(ctx, name, false); err != nil {
		return name, err
	}
	m.emit("preview.removed", name, "", "the branch "+branch+" is gone", map[string]any{"of": domain, "branch": branch})
	return name, nil
}

// ExpirePreviews removes previews not pushed to for longer than their
// production site keeps them, and returns their names. A preview whose
// production site is gone, or no longer has previews, goes too: nothing
// would ever push to it again.
func (m *Manager) ExpirePreviews(ctx context.Context, now time.Time) []string {
	entries, _ := os.ReadDir(filepath.Join(m.Root, "sites"))
	var gone []string
	for _, e := range entries {
		dir := filepath.Join(m.Root, "sites", e.Name())
		mf, err := manifest.Load(dir)
		if err != nil || mf.Staging == nil || mf.Staging.Branch == "" {
			continue
		}
		keep := manifest.DefaultPreviewKeep
		if !fsx.Exists(m.Dir(mf.Staging.Of)) {
			keep = 0
		} else if prod, err := manifest.Load(m.Dir(mf.Staging.Of)); err == nil {
			// A production manifest that does not read keeps the default:
			// a typo there must not delete every preview.
			if prod.Previews == nil {
				keep = 0
			} else {
				keep = prod.Previews.Lifetime()
			}
		}
		if keep > 0 && now.Sub(m.lastPush(e.Name(), dir)) < keep {
			continue
		}
		if _, err := m.Delete(ctx, e.Name(), false); err == nil {
			gone = append(gone, e.Name())
			m.emit("preview.removed", e.Name(), "", "not pushed to for "+keep.String(), map[string]any{"of": mf.Staging.Of, "branch": mf.Staging.Branch})
		}
	}
	return gone
}
