package api

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcimorra/the-middleware/internal/auth"
	goruntime "github.com/dcimorra/the-middleware/internal/runtime"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/relocate"
	"github.com/dcimorra/the-middleware/internal/sites"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// Moving a site to another node of the network. Both ends are here: the
// routes the moving node calls on the target (receive, the last copy,
// commit, discard), and the operator's POST /sites/{domain}/move on the
// source, which runs the move in the background and says each step as an
// event (relocate.*) and in GET /sites/{domain}/move.

// registerRelocateTarget mounts the receiving end. It needs no mesh: the
// routes are what another node calls with the network's secret, and a
// node may call anything but the network's own routes.
func (s *Server) registerRelocateTarget() {
	rc := relocate.Receiver{Root: s.opts.Root}
	bad := func(err error) error {
		switch {
		case errors.Is(err, relocate.ErrExists):
			return Errorf(http.StatusConflict, "%v", err)
		case errors.Is(err, relocate.ErrBadName), errors.Is(err, relocate.ErrNoStaging):
			return Errorf(http.StatusBadRequest, "%v", err)
		}
		return err
	}
	s.Handle("POST", Prefix+"/relocate/{domain}/check", "Whether this node can take a site moving in (body: names, the ones published where it is): the name is free here and every name can be published by this node", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain := r.PathValue("domain")
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%v", relocate.ErrBadName)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Get(domain); err == nil {
			return nil, Errorf(http.StatusConflict, "%s is already a site on this node", domain)
		}
		var body relocate.Needs
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		for _, name := range body.Tasks {
			if err := rc.CheckTask(domain, name, nil); err != nil {
				return nil, Errorf(http.StatusConflict, "%v: the site's task could not come with it", err)
			}
		}
		if len(body.Carried) > 0 && !body.Docker {
			if err := dockerAnswers(r.Context()); err != nil {
				return nil, Errorf(http.StatusConflict, "this node has no working docker (%v) and the site keeps data in docker volumes (%s)", err, strings.Join(body.Carried, ", "))
			}
		}
		if name, ok := relocate.KnownData(domain, body.Data); !ok {
			return nil, Errorf(http.StatusConflict, "this node runs a version that would not keep %s, which the site has where it is: update this node first", name)
		}
		if body.Docker {
			if err := dockerAnswers(r.Context()); err != nil {
				return nil, Errorf(http.StatusConflict, "this node has no working docker (%v) and the site runs in a container: install docker here (run middleware install again), or take the site out of its container before moving it", err)
			}
		}
		if err := receiverFor(rc).CheckNeeds(r.Context(), body); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		if len(body.Public) > 0 {
			// Asked before anything moves: found at the start there, the
			// site would be stopped here with its relay refused.
			if s.portsOptions() == nil {
				return nil, Errorf(http.StatusConflict, "this node does not handle public ports and the site has services with them (%s)", strings.Join(body.Public, ", "))
			}
			asked := &manifest.Manifest{Services: map[string]*manifest.Service{"moving": {Ports: body.Public}}}
			if c := s.portConflicts(domain, asked, false); len(c) > 0 {
				return nil, Errorf(http.StatusConflict, "a public port of the site is taken on this node: %s", strings.TrimPrefix(c[0], "services.moving.ports: "))
			}
		}
		if len(body.Names) == 0 {
			return OK, nil
		}
		if s.publisher == nil {
			return nil, Errorf(http.StatusConflict, "this node publishes nothing: its names would not follow")
		}
		_, no, err := s.publisher.Covers(r.Context(), body.Names)
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		if len(no) > 0 {
			return nil, Errorf(http.StatusConflict, "this node cannot publish %s (no DNS provider for their zone here): its names would not follow; enable the cloudflare module here first", strings.Join(no, ", "))
		}
		return OK, nil
	})
	s.Handle("PUT", Prefix+"/relocate/{domain}", "Receive a site moving in from another node: its bundle (.tar.gz) as the raw body, kept apart until commit", func(w http.ResponseWriter, r *http.Request) (any, error) {
		defer s.receiving()()
		domain := r.PathValue("domain")
		rep, err := rc.Receive(r.Context(), domain, r.Body)
		if err != nil {
			return nil, bad(err)
		}
		// A node from before volumes went with a site sends the folder and
		// would have this one start its containers on empty volumes: said
		// now, at the first copy, while the site still serves where it is.
		if r.URL.Query().Get("carries") == "" {
			if mf, err := manifest.Load(rc.StagingSite(domain)); err == nil {
				if left := relocate.Uncarried(domain, mf); len(left) > 0 {
					_ = rc.Discard(domain)
					return nil, Errorf(http.StatusConflict, "the node %s comes from runs a version that leaves a site's docker volumes behind (%s), and it would start here on empty ones: update that node first", domain, strings.Join(left, ", "))
				}
			}
		}
		return rep, nil
	})
	s.Handle("PUT", Prefix+"/relocate/{domain}/volumes/{volume}", "A docker volume of a site moving in, sent with the site stopped where it is: a gzipped tar as the raw body, written into a new volume of that name (refused if one is here), read back from it and answered as what is now on this disk (digest, files, bytes)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		defer s.receiving()()
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "the volume's body is not gzip: %v", err)
		}
		defer zr.Close()
		sum, err := receiverFor(rc).TakeVolume(r.Context(), r.PathValue("domain"), r.PathValue("volume"), zr)
		if errors.Is(err, relocate.ErrVolumeHere) {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		if err != nil {
			return nil, bad(err)
		}
		return sum, nil
	})
	s.Handle("PUT", Prefix+"/relocate/{domain}/delta", "The last copy of a site moving in: what changed since the first, and the index of what is there", func(w http.ResponseWriter, r *http.Request) (any, error) {
		defer s.receiving()()
		rep, err := rc.ReceiveDelta(r.Context(), r.PathValue("domain"), r.Body)
		if err != nil {
			return nil, bad(err)
		}
		return rep, nil
	})
	s.registerPrepare(rc)
	s.registerMigrate(rc)
	s.Handle("POST", Prefix+"/relocate/{domain}/commit", "Put a received site in place (body: manifest, base64: the one it had before it stopped); the engine starts it", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Manifest string `json:"manifest"`
			// Install: the site's dependencies were not sent (an import
			// leaves node_modules behind, built for the other machine);
			// they are installed here before the site is put in place.
			Install bool `json:"install"`
			// Tasks are the site's task files by name, base64: they run
			// here from the moment it is in place.
			Tasks map[string]string `json:"tasks"`
		}
		defer s.receiving()()
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(body.Manifest)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "manifest: not base64")
		}
		taskFiles, err := relocate.DecodeTasks(body.Tasks)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		domain := r.PathValue("domain")
		if body.Install {
			ictx, icancel := context.WithTimeout(r.Context(), 15*time.Minute)
			_, err := installDeps(ictx, rc.StagingSite(domain))
			icancel()
			if err != nil {
				return nil, Errorf(http.StatusUnprocessableEntity, "installing its dependencies here failed, so it was not put in place: %v", err)
			}
		}
		if err := rc.Commit(domain, raw, taskFiles); err != nil {
			return nil, bad(err)
		}
		_ = relocate.MarkArrived(s.opts.Root, domain, time.Now())
		// Its visits and the rest came with it, placed under the feet of
		// whoever keeps them in memory for a name that was here before.
		if mgr, err := s.sitesManager(); err == nil && mgr.Release != nil {
			mgr.Release(domain)
		}
		if o := s.portsOptions(); o != nil {
			// Its approval came with it, placed under the book's feet.
			o.Book.Release(domain)
		}
		s.emitMove("relocate.arrived", domain, "", "arrived from another node", nil)
		return OK, nil
	})
	s.Handle("POST", Prefix+"/relocate/{domain}/notes", "What the node a site came from says did not travel with it (body: warnings): said here, where the site now is and where whoever looks after it will read it", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain := r.PathValue("domain")
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%v", relocate.ErrBadName)
		}
		var body struct {
			Warnings []string `json:"warnings"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if len(body.Warnings) > 8 {
			body.Warnings = body.Warnings[:8]
		}
		for _, w := range body.Warnings {
			if len(w) > 2000 {
				w = w[:2000]
			}
			s.emitMove("relocate.incomplete", domain, "warn", "arrived without something it had where it was: "+w, nil)
		}
		return OK, nil
	})
	s.Handle("GET", Prefix+"/relocate/leftovers", "What moves left on this node: receptions that never finished, earlier copies a commit set aside (.before-…), sites placed here that were not taken out, and records of moves that never said how they ended; the largest first", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return relocate.Leftovers(s.opts.Root, time.Now()), nil
	})
	s.Handle("POST", Prefix+"/relocate/leftovers/clean", "The operator only: remove the leftovers named (body: paths, as GET lists them); only what is listed now is touched", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator removes leftovers")
		}
		var body struct {
			Paths []string `json:"paths"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, p := range body.Paths {
			if err := relocate.Clean(s.opts.Root, p, time.Now()); err != nil {
				out[p] = err.Error()
			} else {
				out[p] = "removed"
			}
		}
		return map[string]any{"results": out}, nil
	})
	s.Handle("POST", Prefix+"/relocate/{domain}/placed", "A move into this node ended well: forget how to take the site out again", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := rc.Placed(r.PathValue("domain")); err != nil {
			return nil, bad(err)
		}
		return OK, nil
	})
	s.Handle("DELETE", Prefix+"/relocate/{domain}", "Throw away what was received for a site that did not finish moving in: the staging and, when it was put in place and its folder already removed, what it put under data/, with what was there before put back", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := rc.Discard(r.PathValue("domain")); err != nil {
			return nil, bad(err)
		}
		return OK, nil
	})
}

// registerImportToken mounts the operator's way to let a site in from a V1
// house: a token for that one site, two hours, opening only what an
// import calls, and the command to run on the V1 machine with it.
func (s *Server) registerImportToken() {
	s.Handle("POST", Prefix+"/relocate/import-token", "A token for importing one site from a V1 house (body: domain): it opens only that site's import routes, for two hours, and comes with the command to run on the V1 machine", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator makes import tokens")
		}
		var body struct {
			Domain string `json:"domain"`
			// Base is the address the V1 machine reaches this house at;
			// empty is the panel's own.
			Base string `json:"base"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		domain := strings.ToLower(strings.TrimSpace(body.Domain))
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a site's name", body.Domain)
		}
		if mgr, err := s.sitesManager(); err == nil {
			if _, err := mgr.Get(domain); err == nil {
				return nil, Errorf(http.StatusConflict, "%s is already a site here", domain)
			}
		}
		if s.opts.Auth == nil {
			return nil, Errorf(http.StatusNotImplemented, "no token store")
		}
		pub, secret, err := s.opts.Auth.CreateExpiringToken("import "+domain, []string{"import:" + domain}, 2*time.Hour)
		if err != nil {
			return nil, err
		}
		// The address the V1 machine reaches this house at: the one asked
		// for, or this node's own panel domain. Never the request's Host:
		// a node opened from another node's panel is reached through the
		// mesh, and its mesh address (10.66.x.y:46101) is no way in from
		// the V1 machine; the first import printed exactly that.
		base := strings.TrimRight(strings.TrimSpace(body.Base), "/")
		if base == "" && s.opts.Settings != nil {
			if d := s.opts.Settings().PanelDomain; d != "" {
				base = "https://" + d
			}
		}
		if base == "" {
			return nil, Errorf(http.StatusBadRequest, "this node has no panel domain of its own, so the V1 machine has no way to reach it: give the address it can reach (base), or import into a node that has one and move the site here afterwards")
		}
		if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
			base = "https://" + base
		}
		// This house's own release: every release so far is a pre-release,
		// and GitHub's "latest" link skips those.
		release := "latest/download"
		if v := s.opts.Version; strings.HasPrefix(v, "v2.") {
			release = "download/" + v
		}
		cmd := fmt.Sprintf("curl -fsSL -o /tmp/middleware-v2 https://github.com/dcimorra/the-middleware/releases/%s/middleware-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') && chmod +x /tmp/middleware-v2 && /tmp/middleware-v2 import --to %s --token %s --domain %s <the app's folder>", release, base, secret, domain)
		return map[string]any{"token": secret, "expires": pub.Expires, "domain": domain, "command": cmd}, nil
	})
}

// installDeps installs a received Node site's dependencies in its staging
// folder, with the Node the site asks for (installed if need be). The
// lockfile says the tool; with none, npm install. It says what it did.
//
// The dev dependencies always: a site whose start builds it (npm run
// build, then tailwindcss, vite, tsc) needs them, and npm leaves them out
// when the machine says production (NODE_ENV, an .npmrc with omit=dev).
// The first move that installed on the target arrived without tailwindcss
// and failed at the start with "tailwindcss: not found", the install
// having said nothing.
func installDeps(ctx context.Context, dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return "no package.json: nothing to install", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return "node_modules already there", nil
	}
	// The same command the analysis shows before the move (one choice,
	// not two that could say different things).
	argv, _ := relocate.InstallCommand(dir)
	path := os.Getenv("PATH")
	bin := argv[0]
	if goruntime.NodeDir != nil {
		if nd := goruntime.NodeDir(ctx, dir, argv); nd != "" {
			path = nd + string(os.PathListSeparator) + path
			if fileThere(filepath.Join(nd, bin)) {
				bin = filepath.Join(nd, bin)
			}
		}
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "NODE_ENV" && k != "PATH" {
			env = append(env, kv)
		}
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(env, "PATH="+path, "CI=1")
	oomBefore := oomKills()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if len(text) > 600 {
			text = text[len(text)-600:]
		}
		if text == "" {
			text = "(it printed nothing)"
		}
		if why := whyKilled(ctx, err, oomBefore); why != "" {
			return "", fmt.Errorf("%s: %v: %s; last output: %s", strings.Join(argv, " "), err, why, text)
		}
		return "", fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, text)
	}
	last := ""
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			last = l
		}
	}
	tools, _ := os.ReadDir(filepath.Join(dir, "node_modules", ".bin"))
	said := fmt.Sprintf("%s: %s; %d programs in node_modules/.bin", strings.Join(argv, " "), last, len(tools))
	gen, err := prismaGenerate(ctx, dir, cmd.Env)
	if err != nil {
		return "", err
	}
	if gen != "" {
		said += "; " + gen
	}
	return said, nil
}

// prismaGenerate makes Prisma's client again after an install: Prisma
// writes it inside node_modules (.prisma/client), and since Prisma 7 an
// install no longer does. comparaloo arrived with every page that read the
// database failing on "Cannot find module '.prisma/client/default'": its
// client had been generated by hand, long before, in the node_modules the
// move left behind. Only for a site that has a schema and the prisma CLI,
// and only when the install did not make the client itself.
func prismaGenerate(ctx context.Context, dir string, env []string) (string, error) {
	cli := filepath.Join(dir, "node_modules", ".bin", "prisma")
	if !fileThere(cli) || fileThere(filepath.Join(dir, "node_modules", ".prisma", "client", "default.js")) {
		return "", nil
	}
	schema := false
	for _, p := range []string{"prisma/schema.prisma", "schema.prisma", "prisma.config.ts", "prisma.config.js", "prisma.config.mjs"} {
		schema = schema || fileThere(filepath.Join(dir, p))
	}
	if !schema {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, cli, "generate")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 600 {
		text = text[len(text)-600:]
	}
	if err != nil {
		return "", fmt.Errorf("prisma generate, after the install (Prisma's client lives in node_modules and the install no longer makes it): %v: %s", err, text)
	}
	return "prisma generate: its client made again", nil
}

// siteNodeMajor is the Node major the site's command runs on here: the
// one nodeenv gives it, or the machine's own.
func siteNodeMajor(ctx context.Context, dir string, argv []string) string {
	node := "node"
	if goruntime.NodeDir != nil {
		if nd := goruntime.NodeDir(ctx, dir, argv); nd != "" {
			node = filepath.Join(nd, "node")
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, node, "--version").Output()
	if err != nil {
		return ""
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, _, _ := strings.Cut(v, ".")
	if _, err := strconv.Atoi(major); err != nil {
		return ""
	}
	return major
}

// installsThere says a moved site's node_modules is installed on the
// target rather than carried: a Node site on the machine. One in a
// container keeps it, built inside the same image on both sides.
func installsThere(dir string, m *manifest.Manifest) bool {
	if m == nil || m.Run == nil || m.Run.Runtime == "docker" {
		return false
	}
	return fileThere(filepath.Join(dir, "package.json"))
}

func fileThere(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// registerRelocateSource mounts the operator's move: it needs the mesh.
func (s *Server) registerRelocateSource(m *nodes.Mesh) {
	s.recoverMoves(func(id string) (relocate.Target, bool) {
		x, ok := m.Member(id)
		return meshTarget{m: m, x: x}, ok
	})
	s.Handle("POST", Prefix+"/sites/{domain}/move", "Move the site to another node of the network (body: node): copied while it serves, stopped for the last copy, started there, names pointed there, archived here", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Node string `json:"node"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		x, ok := m.Member(body.Node)
		if !ok {
			return nil, Errorf(http.StatusNotFound, "%v", nodes.ErrNotFound)
		}
		if x.ID == m.Self().ID {
			return nil, Errorf(http.StatusBadRequest, "%s is this node", x.Name)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Get(domain); err != nil {
			return nil, err
		}
		// Asked now, not only when the move starts: a target that cannot
		// take it is said in the answer, not in an event a minute later.
		src := siteSource{s: s, mgr: mgr}
		cctx, ccancel := context.WithTimeout(r.Context(), 30*time.Second)
		// Its exports' programs looked for, not run: this answer has
		// seconds, and the move's own check tries them.
		_, err = (&relocate.Mover{Root: s.opts.Root, Source: src, NoTrial: true}).Check(cctx, domain, meshTarget{m: m, x: x})
		ccancel()
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%s cannot take %s: %v", x.Name, domain, err)
		}
		install := false
		if site, err := mgr.Get(domain); err == nil && installsThere(mgr.Dir(domain), site.Manifest) {
			install = true
			// The Node it runs on here goes with it, written in its
			// manifest, when nothing says it yet: a site that asks for none
			// runs on each machine's own node, and the target's was another
			// (24 here, 18 there: a native module built here did not load there).
			if site.Manifest.Node == "" {
				if v := siteNodeMajor(r.Context(), mgr.Dir(domain), site.Manifest.Run.Command); v != "" {
					raw, _ := json.Marshal(v)
					if _, err := mgr.Patch(domain, map[string]json.RawMessage{"node": raw}); err != nil {
						return nil, Errorf(http.StatusInternalServerError, "writing the Node it runs on (%s) in its manifest: %v", v, err)
					}
				}
			}
		}
		_, published := src.Published(domain)
		vols, _ := src.Volumes(r.Context(), domain)
		job, busy := s.newMove(domain, x.Name, movePlan(install, published, len(vols) > 0))
		if busy != nil {
			return nil, Errorf(http.StatusConflict, "%s is already moving to %s (%s)", domain, busy.Node, busy.Step)
		}
		mover := &relocate.Mover{Root: s.opts.Root, Source: src, Journal: &relocate.Journal{Target: x.ID, ID: job.ID}}
		if install {
			mover.Install, mover.Parts = true, relocate.MoveParts
		}
		// Read now, with the site still here: what runs next to it that
		// the move will not carry.
		var left []string
		if cur, err := mgr.Get(domain); err == nil {
			left = s.unmanaged(r.Context(), mgr.Dir(domain), cur.Manifest)
		}
		for _, w := range left {
			s.emitMove("relocate.incomplete", domain, "warn", w, map[string]any{"node": x.Name, "id": job.ID})
		}
		go func() {
			// A move outlives the request that asked for it, for as long as
			// its steps may take by the site's manifest (Budget): a fixed
			// hour was less than its own export and import allowed.
			ctx, cancel := context.WithTimeout(context.Background(), mover.Budget(domain))
			defer cancel()
			rep, err := mover.Move(ctx, domain, meshTarget{m: m, x: x}, func(step, msg string) {
				s.advance(job, step, msg)
				s.emitMove("relocate."+step, domain, "", msg, map[string]any{"node": x.Name, "id": job.ID})
			})
			s.finish(job, rep, err)
			if err != nil {
				s.emitMove("relocate.failed", domain, "error", err.Error(), map[string]any{"node": x.Name, "id": job.ID})
			} else if len(left) > 0 {
				// Said there too, where the site and its agent now are.
				// A node from before this route answers 404, and the
				// warning stays the one said here.
				nctx, ncancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = meshTarget{m: m, x: x}.do(nctx, "POST", "/relocate/"+domain+"/notes", map[string]any{"warnings": left}, nil)
				ncancel()
			}
		}()
		s.moves.mu.Lock()
		c := job.copy()
		s.moves.mu.Unlock()
		return c, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/move/plan", "What moving the site would do, before it does it (?node= the target, which is asked whether it can take it): bytes and files copied, where dependencies are installed and with what, its Node, the names that follow, links that leave its folder, warnings", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		site, err := mgr.Get(domain)
		if err != nil {
			return nil, err
		}
		dir := mgr.Dir(domain)
		src := siteSource{s: s, mgr: mgr}
		install := installsThere(dir, site.Manifest)
		parts := relocate.Parts
		if install {
			parts = relocate.MoveParts
		}
		var names []string
		if _, ok := src.Published(domain); ok {
			names = src.Names(domain)
		}
		a := relocate.Analyze(dir, relocate.Excluding(parts(s.opts.Root, domain), dir, site.Manifest), install, names)
		if cs := foreignContainers(r.Context(), dir); len(cs) > 0 {
			a.Warnings = append(a.Warnings, relocate.Unmanaged(dir, site.Manifest, cs)...)
		}
		if vols, err := src.Volumes(r.Context(), domain); err != nil {
			a.Warnings = append(a.Warnings, fmt.Sprintf("docker could not be asked which volumes are the site's (%v): the move will not start until it can, since it would leave them behind", err))
		} else if len(vols) > 0 {
			a.Volumes = vols
			a.Warnings = append(a.Warnings, fmt.Sprintf("its docker volumes (%s) are copied file for file with the site stopped, which adds their copy to the time it is down, and stay on this node, listed, until you remove them. A database's files are only good on a machine of the same kind of processor: to move between amd64 and arm64, take a copy of the database first (its backups go with the site) and restore it there if it does not start", strings.Join(vols, ", ")))
		}
		out := map[string]any{"analysis": a}
		if install && a.Node == "" {
			if v := siteNodeMajor(r.Context(), dir, site.Manifest.Run.Command); v != "" {
				// Written in its manifest when the move starts, so the
				// target runs it on the same major.
				out["node_pinned"] = v
			}
		}
		if id := r.URL.Query().Get("node"); id != "" {
			x, ok := m.Member(id)
			if !ok {
				return nil, Errorf(http.StatusNotFound, "%v", nodes.ErrNotFound)
			}
			out["node"] = x.Name
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			// The exports that wait for the stop: their programs looked for
			// where they run, and said whether each has worked here before
			// or will be tried, at the move, before the site stops.
			mover := &relocate.Mover{Root: s.opts.Root, Source: src, NoTrial: true, Note: func(said string) { out["exports"] = said }}
			if _, err := mover.Check(ctx, domain, meshTarget{m: m, x: x}); err != nil {
				out["check_error"] = err.Error()
			}
		}
		return out, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/move", "The site's last move to another node: each operation with its state, and how it ended", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if list := s.listMoves(domain); len(list) > 0 {
			return list[0], nil
		}
		return nil, Errorf(http.StatusNotFound, "%s has not been moved from this node", domain)
	})
	s.Handle("GET", Prefix+"/moves", "The moves from this node to others, the newest first (?domain= one site's): each operation with its state, its times and its error", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.listMoves(r.URL.Query().Get("domain")), nil
	})
}

func (s *Server) emitMove(typ, domain, level, msg string, data map[string]any) {
	if s.opts.Bus == nil {
		return
	}
	s.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "engine", Level: level, Message: msg, Data: data})
}

// siteSource is this node's side of a move.
type siteSource struct {
	s   *Server
	mgr *sites.Manager
}

func (src siteSource) Stop(ctx context.Context, domain string) ([]byte, error) {
	return src.mgr.StopForMove(ctx, domain)
}

func (src siteSource) Start(domain string, m []byte) error { return src.mgr.Resume(domain, m) }

func (src siteSource) Published(domain string) (string, bool) {
	if src.s.publisher == nil {
		return "", false
	}
	door, ok := src.s.publisher.Published(domain)
	return string(door), ok
}

func (src siteSource) Names(domain string) []string {
	if src.s.publisher == nil {
		return nil
	}
	return src.s.publisher.HostsOf(domain)
}

func (src siteSource) Unpublish(ctx context.Context, domain string) {
	if src.s.publisher != nil {
		src.s.publisher.Unpublish(ctx, domain)
	}
}

// Export implements relocate.Exporter: each worker's move.export where it
// runs here, inside its container, with the site held and the worker up
// (StopForMove keeps it).
func (src siteSource) Export(ctx context.Context, domain string, m []byte, workers []string) (string, error) {
	mf, err := manifest.Parse(m)
	if err != nil {
		return "", err
	}
	return relocate.ExportHere(ctx, src.s.opts.Root, src.mgr.Dir(domain), domain, mf, workers)
}

// Rehearse implements relocate.Rehearser: the exports that wait for the
// stop, tried where they will run while the site serves.
func (src siteSource) Rehearse(ctx context.Context, domain string, m []byte, workers []string, trial bool) (string, error) {
	mf, err := manifest.Parse(m)
	if err != nil {
		return "", err
	}
	return relocate.RehearseHere(ctx, src.s.opts.Root, src.mgr.Dir(domain), domain, mf, workers, trial, goruntime.Docker{})
}

// Tasks implements relocate.Tasker.
func (src siteSource) Tasks(domain string) (map[string][]byte, error) {
	return tasks.OfSite(src.s.opts.Root, domain)
}

// HoldTasks implements relocate.Tasker.
func (src siteSource) HoldTasks(domain string) ([]string, error) {
	files, err := tasks.OfSite(src.s.opts.Root, domain)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, name := range relocate.SortedNames(files) {
		t, err := tasks.Parse(files[name])
		if err != nil || t.Disabled {
			continue
		}
		if err := tasks.SetDisabled(src.s.opts.Root, name, true); err != nil {
			return held, fmt.Errorf("task %s: %w", name, err)
		}
		held = append(held, name)
	}
	return held, nil
}

// ReleaseTasks implements relocate.Tasker.
func (src siteSource) ReleaseTasks(names []string) error {
	var failed []string
	for _, name := range names {
		if err := tasks.SetDisabled(src.s.opts.Root, name, false); err != nil {
			failed = append(failed, name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

func (src siteSource) Archive(ctx context.Context, domain string, m []byte) (string, error) {
	path, err := src.mgr.ArchiveMoved(ctx, domain, m)
	// What the owner approved went with the site. Left here it would be
	// waiting for whatever is next created under the name, which may ask
	// the same ports without anybody having said yes to it.
	if o := src.s.ports; err == nil && o != nil && o.Book != nil {
		_ = o.Book.Revoke(domain)
	}
	return path, err
}

// volumeDocker is the docker a site's volumes are read from and written
// to; the tests give it one of their own.
var volumeDocker = func(root string) goruntime.Docker {
	return goruntime.Docker{Data: filepath.Join(root, "data")}
}

// receiverFor is rc with the docker volumes are written through.
func receiverFor(rc relocate.Receiver) relocate.Receiver {
	d := volumeDocker(rc.Root)
	rc.Docker = &d
	return rc
}

// Volumes implements relocate.Carrier: every docker volume that is the
// site's on this node, labelled, claimed or named in its manifest.
func (src siteSource) Volumes(ctx context.Context, domain string) ([]string, error) {
	var mf *manifest.Manifest
	if site, err := src.mgr.Get(domain); err == nil {
		mf = site.Manifest
	}
	d := volumeDocker(src.s.opts.Root)
	if d.Bin == "" {
		if _, err := exec.LookPath("docker"); err != nil {
			// No docker on this machine: no volumes on it.
			return nil, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return relocate.DockerCarrier{Docker: d}.SiteVolumes(ctx, domain, mf)
}

// VolumeUsers implements relocate.Carrier.
func (src siteSource) VolumeUsers(ctx context.Context, volume string) ([]string, error) {
	return volumeDocker(src.s.opts.Root).VolumeUsers(ctx, volume)
}

// ReadVolume implements relocate.Carrier.
func (src siteSource) ReadVolume(ctx context.Context, volume string, w io.Writer) error {
	return volumeDocker(src.s.opts.Root).ReadVolume(ctx, volume, w)
}

// Release implements relocate.Releaser.
func (src siteSource) Release(domain string) {
	if src.mgr.Release != nil {
		src.mgr.Release(domain)
	}
}

// RemoveVolumes implements relocate.Volumer: the volumes of a site that
// moved away, once its archive keeps their dumps.
func (src siteSource) RemoveVolumes(ctx context.Context, vols []string) error {
	return relocate.DockerVolumes{}.RemoveVolumes(ctx, vols)
}

// meshTarget is a node of the network as a move's target.
type meshTarget struct {
	m *nodes.Mesh
	x nodes.Member
	// call, when set, replaces the mesh: the tests reach another server's
	// real routes over plain HTTP. A body that is an io.Reader is a
	// bundle, sent as it is.
	call func(ctx context.Context, method, path string, body, out any) error
}

func (t meshTarget) Name() string { return t.x.Name }

func (t meshTarget) do(ctx context.Context, method, path string, body, out any) error {
	if t.call != nil {
		return t.call(ctx, method, path, body, out)
	}
	return t.m.Call(ctx, t.x, method, path, body, out)
}

func (t meshTarget) stream(ctx context.Context, method, path string, body io.Reader) error {
	if t.call != nil {
		return t.call(ctx, method, path, body, nil)
	}
	return t.m.Stream(ctx, t.x, method, path, "application/gzip", body, nil)
}

// jobStart is what a node answers to the POST that starts one of its jobs
// for a move (an install, an import).
type jobStart struct {
	Started bool   `json:"started"`
	Running bool   `json:"running"`
	ID      string `json:"id"`
}

// startJob posts to path and says the job it started there.
func (t meshTarget) startJob(ctx context.Context, path, what, domain string, body any) (string, error) {
	var st jobStart
	if err := t.do(ctx, "POST", path, body, &st); err != nil {
		return "", err
	}
	if !st.Started && st.Running {
		// Polled, it would answer for an earlier attempt's job, which runs
		// on a staging that was received again since: whatever it ends
		// with is not this move's.
		return "", fmt.Errorf("an earlier %s of %s is still running on %s: move it again once that one ends", what, domain, t.x.Name)
	}
	return st.ID, nil
}

// waitJob asks path every few seconds until the job id ends there, each a
// short request: the job runs there on its own, and no connection is held
// open for its minutes. One unanswered question is not the job failing;
// the move's own time (its context) is what gives up on a node that does
// not answer. A node that answers it has no such job, though, has
// forgotten it: it restarted, and a job lives in its memory. Asked again
// it says the same until the move's hour is out, and the site was stopped
// here for that hour before this, waiting for a job nobody ran.
func (t meshTarget) waitJob(ctx context.Context, path, what, id string) (string, error) {
	for {
		var j prepareJob
		err := t.do(ctx, "GET", path, nil, &j)
		var re *nodes.RemoteError
		switch {
		case errors.As(err, &re) && re.Status == http.StatusNotFound:
			return "", fmt.Errorf("%s no longer knows of the %s (it restarted while it ran): it will never end there", t.x.Name, what)
		case err != nil:
		case id != "" && j.ID != id:
			return "", fmt.Errorf("%s answers for another %s than the one this move started (it restarted, and something started one since)", t.x.Name, what)
		case j.Done && j.Error != "":
			return "", errors.New(j.Error)
		case j.Done:
			return j.Output, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(jobPoll):
		}
	}
}

// jobPoll is how often waitJob asks; the tests make it short.
var jobPoll = 3 * time.Second

// older says a node that answers 404 to a move's route runs a version
// without moves, which is what to tell the operator.
func (t meshTarget) older(err error) error {
	var re *nodes.RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound && strings.Contains(re.Message, "no such route") {
		return fmt.Errorf("%s runs a version that cannot receive sites: update it first", t.x.Name)
	}
	// A check that says more than it knows (the volumes and ports a move
	// asks about now) is refused whole by an older node's strict reading.
	if errors.As(err, &re) && re.Status == http.StatusBadRequest && strings.Contains(re.Message, "unknown field") {
		return fmt.Errorf("%s runs an older version that cannot check what this site needs (%s): update it first", t.x.Name, re.Message)
	}
	return err
}

func (t meshTarget) Send(ctx context.Context, domain string, body io.Reader) error {
	// carries: this node sends the site's docker volumes after its stop,
	// which the target otherwise refuses a site that has them for.
	return t.older(t.stream(ctx, "PUT", "/relocate/"+domain+"?carries=1", body))
}

// TakeVolume implements relocate.VolumeTaker: the tar goes gzipped, a
// database's files being mostly air, and the node answers with what it
// read back from the volume it wrote.
func (t meshTarget) TakeVolume(ctx context.Context, domain, volume string, body io.Reader) (relocate.VolumeSum, error) {
	var sum relocate.VolumeSum
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		_, err := io.Copy(zw, body)
		if err == nil {
			err = zw.Close()
		}
		pw.CloseWithError(err)
	}()
	defer pr.CloseWithError(errors.New("the target stopped reading"))
	path := "/relocate/" + domain + "/volumes/" + volume
	var err error
	if t.call != nil {
		err = t.call(ctx, "PUT", path, io.Reader(pr), &sum)
	} else {
		err = t.m.Stream(ctx, t.x, "PUT", path, "application/gzip", pr, &sum)
	}
	return sum, t.older(err)
}

func (t meshTarget) SendDelta(ctx context.Context, domain string, body io.Reader) error {
	return t.older(t.stream(ctx, "PUT", "/relocate/"+domain+"/delta", body))
}

func (t meshTarget) Commit(ctx context.Context, domain string, m []byte, taskFiles map[string][]byte) error {
	body := map[string]any{"manifest": base64.StdEncoding.EncodeToString(m)}
	if len(taskFiles) > 0 {
		body["tasks"] = relocate.EncodeTasks(taskFiles)
	}
	return t.do(ctx, "POST", "/relocate/"+domain+"/commit", body, nil)
}

func (t meshTarget) Prepare(ctx context.Context, domain string, dirs []string) (string, error) {
	id, err := t.startJob(ctx, "/relocate/"+domain+"/prepare", "install", domain, map[string]any{"dirs": dirs})
	if err != nil {
		return "", t.older(err)
	}
	return t.waitJob(ctx, "/relocate/"+domain+"/prepare", "install of "+domain, id)
}

// Migrate implements relocate.Migrator: the node runs the workers' imports
// on its own and is asked how it goes, as Prepare.
func (t meshTarget) Migrate(ctx context.Context, domain string, m []byte, workers []string) (string, error) {
	body := map[string]any{"manifest": base64.StdEncoding.EncodeToString(m), "workers": workers}
	id, err := t.startJob(ctx, "/relocate/"+domain+"/migrate", "data import", domain, body)
	if err != nil {
		var re *nodes.RemoteError
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return "", fmt.Errorf("%s cannot import a worker's data (update it to a version that can)", t.x.Name)
		}
		return "", err
	}
	return t.waitJob(ctx, "/relocate/"+domain+"/migrate", "data import of "+domain, id)
}

func (t meshTarget) Discard(ctx context.Context, domain string) error {
	return t.do(ctx, "DELETE", "/relocate/"+domain, nil, nil)
}

func (t meshTarget) Repair(ctx context.Context, domain, why string) (string, error) {
	if err := t.do(ctx, "POST", "/relocate/"+domain+"/repair", map[string]string{"why": why}, nil); err != nil {
		var re *nodes.RemoteError
		if errors.As(err, &re) && (re.Status == http.StatusConflict || re.Status == http.StatusNotFound) {
			return "", relocate.ErrNoRepair
		}
		return "", err
	}
	return t.waitJob(ctx, "/relocate/"+domain+"/repair", "repair of "+domain, "")
}

func (t meshTarget) Finish(ctx context.Context, domain string) error {
	return t.do(ctx, "POST", "/relocate/"+domain+"/placed", nil, nil)
}

func (t meshTarget) Remove(ctx context.Context, domain string) error {
	return t.do(ctx, "DELETE", "/sites/"+domain, nil, nil)
}

func (t meshTarget) Check(ctx context.Context, domain string, need relocate.Needs) error {
	return t.older(t.do(ctx, "POST", "/relocate/"+domain+"/check", need, nil))
}

func (t meshTarget) Ready(ctx context.Context, domain string) (bool, string, error) {
	var site sites.Site
	if err := t.do(ctx, "GET", "/sites/"+domain, nil, &site); err != nil {
		// Said apart from a node that does not answer: a move put right
		// after a restart undoes itself on the first and waits on the other.
		var re *nodes.RemoteError
		if errors.As(err, &re) && re.Status == http.StatusNotFound && !strings.Contains(re.Message, "no such route") {
			return false, "", fmt.Errorf("%w: %s", relocate.ErrNotThere, re.Message)
		}
		return false, "", err
	}
	ok, why := SiteReady(site)
	return ok, why, nil
}

func (t meshTarget) Publish(ctx context.Context, domain, door string) error {
	return t.do(ctx, "POST", "/sites/"+domain+"/publish", map[string]any{"door": inbound.Door(door), "over": true}, nil)
}

// SiteReady says a site answers as it should: no error, and its process
// alive, or its workers for a project, or nothing to run for a site that
// serves files.
func SiteReady(s sites.Site) (bool, string) {
	if s.Error != "" {
		return false, s.Error
	}
	if s.Alive {
		return true, ""
	}
	switch s.Mode {
	case manifest.ModeStatic:
		return true, ""
	case manifest.ModeProject:
		for name, w := range s.Workers {
			if !w.Alive {
				return false, "worker " + name + " is not up"
			}
		}
		return len(s.Workers) > 0, "no worker is up yet"
	}
	return false, "not up yet"
}

// dockerAnswers says this machine has a docker whose daemon answers.
func dockerAnswers(ctx context.Context) error {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		return fmt.Errorf("the daemon does not answer: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// foreignContainers are the containers running on this node that go with
// the site in dir and are not the house's; var so a test without docker
// can say what it finds.
var foreignContainers = func(ctx context.Context, dir string) []goruntime.Foreign {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return goruntime.Docker{}.ForeignContainers(ctx, dir)
}

// unmanaged are a move's warnings about what runs next to the site and
// will stay here (relocate.Unmanaged).
func (s *Server) unmanaged(ctx context.Context, dir string, mf *manifest.Manifest) []string {
	return relocate.Unmanaged(dir, mf, foreignContainers(ctx, dir))
}
