package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/shots"
	"github.com/dcimorra/the-middleware/internal/sites"
)

func (s *Server) registerSites() {
	s.registerStaging()
	p := Prefix + "/sites"
	s.Handle("GET", p, "List every site folder, unmanaged ones included", s.sitesList)
	s.Handle("POST", p, "Create a site from a manifest or a template (static, node, go, docker, fastapi, django, php, astro)", s.sitesCreate)
	s.Handle("GET", p+"/{domain}", "One site", s.sitesGet)
	s.Handle("PUT", p+"/{domain}/manifest", "Replace the manifest", s.sitesPutManifest)
	s.Handle("PATCH", p+"/{domain}/manifest", "Merge top-level keys into the manifest; null removes a key", s.sitesPatchManifest)
	s.Handle("POST", p+"/{domain}/validate", "Check a manifest (the body, or the one on disk) without writing or deploying it: schema, paths, names, Dockerfiles", s.sitesValidate)
	s.Handle("DELETE", p+"/{domain}", "Delete the folder, or move it to data/archive/ with ?archive=true", s.sitesDelete)
	s.Handle("POST", p+"/{domain}/restart", "Force a handover", s.sitesRestart)
	s.Handle("POST", p+"/{domain}/exec", "Run a command where the site runs and wait for it", s.sitesExec)
	s.Handle("GET", p+"/{domain}/manifest/history", "The site's kept manifests, newest first", s.historyList)
	s.Handle("GET", p+"/{domain}/manifest/history/{id}", "One kept manifest as it was written", s.historyGet)
	s.Handle("POST", p+"/{domain}/manifest/history/{id}/restore", "Write a kept manifest back", s.historyRestore)
	s.Handle("POST", p+"/{domain}/workers/{name}/restart", "Restart one worker now", s.workerRestart)
	s.Handle("POST", p+"/{domain}/services/{name}/restart", "A new generation of one service now (a rebuild for a built image); the others are not touched", s.serviceRestart)
	s.Handle("POST", p+"/{domain}/services/{name}/stop", "Stop one service (writes services.<name>.disabled); its names answer the stopped page", s.serviceStop)
	s.Handle("POST", p+"/{domain}/services/{name}/start", "Start one service (clears services.<name>.disabled)", s.serviceStart)
	s.Handle("POST", p+"/{domain}/services/{name}/hold", "Put one service's names behind the holding page, or take them out (services.<name>.hold)", s.serviceHold)
	s.Handle("GET", p+"/{domain}/routes", "Every name of the site: host and prefix → service → port → state", s.sitesRoutes)
	s.Handle("POST", p+"/{domain}/workers/{name}/stop", "Stop one worker (writes workers.<name>.disabled)", s.workerStop)
	s.Handle("POST", p+"/{domain}/workers/{name}/start", "Start one worker (clears workers.<name>.disabled)", s.workerStart)
	s.Handle("POST", p+"/{domain}/stop", "Stop the site (disabled: true)", s.sitesStop)
	s.Handle("POST", p+"/{domain}/start", "Start the site (clears disabled)", s.sitesStart)
	s.Handle("POST", p+"/{domain}/hold", "Put the site behind the holding page, or take it out", s.sitesHold)
	s.Handle("GET", p+"/{domain}/logs", "The site's or a worker's log as text; ?follow=1 streams", s.sitesLogs)
	s.Handle("GET", p+"/{domain}/files", "List a folder inside the site", s.sitesFiles)
	s.Handle("GET", p+"/{domain}/file", "Read a file inside the site", s.sitesReadFile)
	s.Handle("PUT", p+"/{domain}/file", "Write a file inside the site (raw body)", s.sitesWriteFile)
	s.Handle("POST", p+"/{domain}/deploy", "Run deploy.github.build and write the pointer", s.sitesDeploy)
	s.Handle("GET", p+"/{domain}/env", "The environment the instance is born with, secrets masked", s.sitesEnv)
	s.Handle("POST", p+"/{domain}/names/{name}/main", "Make one of the site's names its main one: the site is renamed to it, the old name stays as an alias, nothing is unpublished", s.sitesSetMain)
	s.Handle("DELETE", p+"/{domain}/names/{name}", "Take a name away from the site (an alias, or a service's domains on that host); never its main name", s.sitesRemoveName)
	s.Handle("POST", p+"/{domain}/rename", "Rename a site (body: to): folder, HOME, logs, conversations and cost move with it; published names follow", s.sitesRename)
	s.Handle("GET", p+"/{domain}/download.zip", "The site folder as a zip, without what an install or a build gives back (?all=1 for everything)", s.sitesDownload)
	s.Handle("GET", Prefix+"/archive", "Archived sites, newest first", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		list := m.Archives()
		if list == nil {
			list = []sites.Archived{}
		}
		return list, nil
	})
	s.Handle("POST", Prefix+"/archive/{name}/restore", "Put an archived site back in sites/", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		return m.Restore(context.Background(), r.PathValue("name"))
	})
	s.Handle("DELETE", Prefix+"/archive/{name}", "Delete an archive for good", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if err := m.DeleteArchive(r.PathValue("name")); err != nil {
			return nil, err
		}
		return OK, nil
	})
	s.Handle("GET", Prefix+"/archive/{name}/download", "Download an archive (.tar.gz)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		path, _, err := m.ArchivePath(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("name")+`"`)
		http.ServeFile(w, r, path)
		return Handled, nil
	})
}

func (s *Server) sitesManager() (*sites.Manager, error) {
	if s.opts.Sites == nil {
		return nil, Errorf(http.StatusNotImplemented, "no sites manager")
	}
	return s.opts.Sites, nil
}

func (s *Server) sitesList(w http.ResponseWriter, r *http.Request) (any, error) {
	if c := auth.FromContext(r.Context()); c.Delegate() {
		return s.delegateSites(r.Context(), c)
	}
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	return m.List()
}

func (s *Server) sitesGet(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Get(domain)
}

func (s *Server) serviceRestart(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	if err := m.RestartService(r.Context(), domain, r.PathValue("name")); err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return OK, nil
}

func (s *Server) sitesRoutes(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Routes(domain)
}

func (s *Server) sitesValidate(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return nil, Errorf(http.StatusRequestEntityTooLarge, "a manifest is at most 1 MB")
	}
	// Whoever sees one site and not the house is told a name is taken,
	// not by whom: the rest of the house stays invisible to them.
	c := auth.FromContext(r.Context())
	hide := c.Delegate() || c.Turn != "" || len(c.Scopes) > 0
	return m.Validate(domain, bytes.TrimSpace(data), hide)
}

func (s *Server) sitesCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	var body struct {
		Domain   string             `json:"domain"`
		Manifest *manifest.Manifest `json:"manifest"`
		Template string             `json:"template"`
		// Publish defaults to true when a DNS provider is enabled: what
		// "create a site" means is a site people can reach. Door is the
		// door to publish on; empty is the door for new sites.
		Publish *bool        `json:"publish"`
		Door    inbound.Door `json:"door"`
		// Repository makes the site from a GitHub repository (owner/name
		// or its address): the GitHub module must be on. Template then
		// overrides what the repository's root says it is; Branch, its
		// default branch.
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSpace(body.Domain))
	fromRepo := strings.TrimSpace(body.Repository) != ""
	if fromRepo {
		s.mu.Lock()
		src := s.repos
		s.mu.Unlock()
		if src == nil || !src.Enabled() {
			return nil, Errorf(http.StatusConflict, "making a site from a repository needs the GitHub module on")
		}
		if body.Manifest != nil {
			return nil, Errorf(http.StatusBadRequest, "a site from a repository takes its manifest from the repository")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		repo, err := src.Inspect(ctx, body.Repository)
		cancel()
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		body.Manifest, _ = sites.RepoManifest(repo, strings.TrimSpace(body.Branch), body.Template)
	}
	site, err := m.Create(domain, body.Manifest, body.Template)
	if err != nil {
		return nil, err
	}
	if fromRepo {
		// The folder has its site.json and nothing else: the first deploy
		// clones the repository into it, builds it and starts it.
		s.mu.Lock()
		deployer := s.deployer
		s.mu.Unlock()
		if deployer != nil {
			if _, handled, err := deployer(domain); !handled || err != nil {
				s.opts.Bus.Emit(events.Event{Type: "deploy.failed", Subject: domain, Actor: "operator", Level: "error", Message: fmt.Sprintf("the first deploy from %s did not start: %v", body.Repository, err)})
			}
		}
	}
	out := map[string]any{}
	raw, _ := json.Marshal(site)
	_ = json.Unmarshal(raw, &out)
	want := s.dnsEnabled != nil && s.dnsEnabled()
	if body.Publish != nil {
		want = *body.Publish
	}
	if want && s.publisher != nil && !manifest.IsProject(domain) {
		// In the background: the engine has to see the folder first,
		// and a certificate for the ports door takes the CA's time. The
		// outcome is in GET /publish and in publish.* events.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := s.publisher.Publish(ctx, domain, body.Door); err != nil {
				s.opts.Bus.Emit(events.Event{Type: "publish.failed", Subject: domain, Actor: "operator", Level: "error", Message: err.Error()})
			}
		}()
		out["publishing"] = true
	}
	WriteJSON(w, http.StatusCreated, out)
	return Handled, nil
}

func (s *Server) sitesPutManifest(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var mf manifest.Manifest
	if err := ReadJSON(r, &mf); err != nil {
		return nil, err
	}
	if next, err := json.Marshal(&mf); err == nil {
		if err := s.checkDelegateManifest(r, domain, next); err != nil {
			return nil, err
		}
	}
	return m.Update(domain, &mf)
}

func (s *Server) sitesPatchManifest(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var partial map[string]json.RawMessage
	if err := ReadJSON(r, &partial); err != nil {
		return nil, err
	}
	if auth.FromContext(r.Context()).Delegate() {
		next, err := s.patchedManifest(domain, partial)
		if err != nil {
			return nil, err
		}
		if err := s.checkDelegateManifest(r, domain, next); err != nil {
			return nil, err
		}
	}
	return m.Patch(domain, partial)
}

func (s *Server) sitesDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	keep := isTrue(r.URL.Query().Get("archive"))
	// Off the internet first, while the engine still knows its names: a
	// deleted site whose record and tunnel rule stay behind answers
	// "nothing here" for as long as nobody notices. A name that cannot
	// be removed now is retried by the publisher, not forgotten.
	var unpublished []inbound.Line
	if s.publisher != nil {
		if _, ok := s.publisher.Published(domain); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			unpublished = s.publisher.Unpublish(ctx, domain)
			cancel()
		}
	}
	// Packing a large site takes minutes: it must not stop halfway because
	// the browser tab went away.
	dest, err := m.Delete(context.Background(), domain, keep)
	if err != nil {
		return nil, err
	}
	if s.opts.Auth != nil {
		_ = s.opts.Auth.ForgetSite(domain)
	}
	out := map[string]any{"ok": true, "archived": keep}
	if unpublished != nil {
		out["unpublished"] = unpublished
	}
	if dest != "" {
		out["path"] = dest
	}
	return out, nil
}

func (s *Server) sitesRestart(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	if err := m.Restart(r.Context(), domain); err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return OK, nil
}

func (s *Server) sitesExec(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Service string   `json:"service"`
		Stream  bool     `json:"stream"`
		Timeout string   `json:"timeout"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	// A minute by default and ten at most: this answers within one
	// request, and anything longer belongs in a task or a deploy, which
	// do not hang off a browser tab. A stream is a terminal's (`middleware
	// exec` from a site's turn): an install there takes what it takes, up
	// to an hour.
	timeout, most := time.Minute, 10*time.Minute
	if body.Stream {
		timeout, most = 30*time.Minute, time.Hour
	}
	if body.Timeout != "" {
		d, err := time.ParseDuration(body.Timeout)
		if err != nil || d <= 0 || d > most {
			return nil, Errorf(http.StatusBadRequest, "timeout: %q is not a duration up to %s", body.Timeout, most)
		}
		timeout = d
	}
	if (body.Command == "") == (len(body.Args) == 0) {
		return nil, Errorf(http.StatusBadRequest, "exec: send command (run by sh) or args (run as they are), one of them")
	}
	if c := auth.FromContext(r.Context()); !body.Stream && (c.Turn != "" || len(c.Scopes) > 0) {
		return nil, Errorf(http.StatusForbidden, "exec: a turn or a service door streams (stream: true), into a container only")
	}
	if !body.Stream {
		if len(body.Args) > 0 {
			return nil, Errorf(http.StatusBadRequest, "exec: args go with stream")
		}
		return m.Exec(r.Context(), domain, body.Service, body.Command, timeout)
	}
	argv := body.Args
	if body.Command != "" {
		argv = []string{"sh", "-c", body.Command}
	}
	// The exit code goes in a trailer: it is known only once the output,
	// already sent, has ended.
	out := &streamOut{w: w}
	c := auth.FromContext(r.Context())
	code, timedOut, err := m.Run(r.Context(), domain, body.Service, argv, timeout, out, c.Turn != "" || len(c.Scopes) > 0)
	if err != nil && !out.started {
		return nil, err
	}
	out.start()
	w.Header().Set(ExitCodeTrailer, strconv.Itoa(code))
	if timedOut {
		w.Header().Set(ExecErrorTrailer, "timed out after "+timeout.String())
	} else if err != nil {
		w.Header().Set(ExecErrorTrailer, err.Error())
	}
	return Handled, nil
}

// The trailers a streamed exec ends with.
const (
	ExitCodeTrailer  = "X-Exit-Code"
	ExecErrorTrailer = "X-Exec-Error"
)

// streamOut sends a command's output as it comes, the headers on its
// first byte, so an error before the command ran is still a JSON error.
type streamOut struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	started bool
}

func (o *streamOut) start() {
	if o.started {
		return
	}
	o.started = true
	o.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	o.w.Header().Set("Trailer", ExitCodeTrailer+", "+ExecErrorTrailer)
	o.w.Header().Set("X-Content-Type-Options", "nosniff")
	o.w.WriteHeader(http.StatusOK)
}

func (o *streamOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.start()
	n, err := o.w.Write(p)
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (s *Server) historyList(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.History(domain)
}

func (s *Server) historyGet(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	data, err := m.Version(domain, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	// As written, byte for byte: a diff against today's must not show
	// changes a re-encoding made.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
	return Handled, nil
}

func (s *Server) historyRestore(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	if auth.FromContext(r.Context()).Delegate() {
		next, err := m.Version(domain, r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		if err := s.checkDelegateManifest(r, domain, next); err != nil {
			return nil, err
		}
	}
	return m.RestoreVersion(domain, r.PathValue("id"))
}

func (s *Server) workerRestart(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	if err := m.RestartWorker(r.Context(), domain, r.PathValue("name")); err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return OK, nil
}

func (s *Server) workerStop(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setWorkerDisabled(r, true)
}

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setWorkerDisabled(r, false)
}

func (s *Server) setWorkerDisabled(r *http.Request, disabled bool) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.SetWorkerDisabled(domain, r.PathValue("name"), disabled)
}

func (s *Server) serviceStop(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setServiceDisabled(r, true)
}

func (s *Server) serviceStart(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setServiceDisabled(r, false)
}

func (s *Server) setServiceDisabled(r *http.Request, disabled bool) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.SetServiceDisabled(domain, r.PathValue("name"), disabled)
}

func (s *Server) serviceHold(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var hold manifest.Hold
	if err := ReadJSON(r, &hold); err != nil {
		return nil, err
	}
	return m.SetServiceHold(domain, r.PathValue("name"), hold)
}

func (s *Server) sitesStop(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setDisabled(r, true)
}

func (s *Server) sitesStart(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setDisabled(r, false)
}

func (s *Server) setDisabled(r *http.Request, disabled bool) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.SetDisabled(domain, disabled)
}

func (s *Server) sitesHold(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var hold manifest.Hold
	if err := ReadJSON(r, &hold); err != nil {
		return nil, err
	}
	return m.SetHold(domain, hold)
}

func (s *Server) sitesLogs(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	lines := 200
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100000 {
			return nil, Errorf(http.StatusBadRequest, "lines: want 0..100000")
		}
		lines = n
	}
	worker := q.Get("worker")
	if svc := q.Get("service"); svc != "" {
		worker = "service:" + svc
		if isTrue(q.Get("build")) {
			worker = "build:" + svc
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !isTrue(q.Get("follow")) {
		out, err := m.Logs(domain, worker, lines)
		if err != nil {
			return nil, err
		}
		_, _ = io.WriteString(w, out)
		return Handled, nil
	}
	if _, err := m.LogPath(domain, worker); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_ = m.Follow(r.Context(), domain, worker, lines, w)
	return Handled, nil
}

func (s *Server) sitesFiles(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Files(domain, r.URL.Query().Get("path"))
}

func (s *Server) sitesReadFile(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		return nil, Errorf(http.StatusBadRequest, "path: required")
	}
	data, err := m.ReadFile(domain, p)
	if err != nil {
		return nil, err
	}
	// Always a download, never rendered: a site's HTML served from the
	// panel host would run with the panel's cookies.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
	return Handled, nil
}

func (s *Server) sitesWriteFile(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		return nil, Errorf(http.StatusBadRequest, "path: required")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sites.MaxFileSize+1))
	if err != nil {
		return nil, Errorf(http.StatusRequestEntityTooLarge, "body: %v", err)
	}
	if isManifestPath(p) {
		if err := s.checkDelegateManifest(r, domain, data); err != nil {
			return nil, err
		}
	}
	if err := m.WriteFile(domain, p, data); err != nil {
		return nil, err
	}
	return OK, nil
}

func (s *Server) sitesDeploy(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	// A deployer set by the wiring (the github module's runner, which
	// fetches the repository first) takes the request when it wants it;
	// otherwise the manager builds what is already in the folder.
	s.mu.Lock()
	deployer := s.deployer
	s.mu.Unlock()
	if deployer != nil {
		if out, handled, err := deployer(domain); handled {
			return out, err
		}
	}
	id, err := m.Deploy(domain)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "build": id}, nil
}

func (s *Server) sitesEnv(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	env, err := m.ResolvedEnv(domain, r.URL.Query().Get("service"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"resolved": env}, nil
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s *Server) sitesRename(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	from, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		To string `json:"to"`
		// Publish puts the new name on the door for new sites even when
		// the old one was not published: a project being given a domain
		// was never published, and the domain is what it is given for.
		Publish bool `json:"publish"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	to := strings.ToLower(strings.TrimSpace(body.To))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The old names leave the internet with the old site, and the new
	// ones are published on the same door: a rename that left DNS on the
	// old name would be half a rename.
	var door inbound.Door
	published := false
	if s.publisher != nil {
		if door, published = s.publisher.Published(from); published {
			s.publisher.Unpublish(ctx, from)
		}
	}
	site, err := m.Rename(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if s.opts.Auth != nil {
		_ = s.opts.Auth.RenameSite(from, to)
	}
	if body.Publish && !published && s.publisher != nil && !manifest.IsProject(to) {
		published = true
	}
	if published {
		go func() {
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer pcancel()
			_, _ = s.publisher.Publish(pctx, to, door)
		}()
	}
	return site, nil
}

func (s *Server) sitesSetMain(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	from, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	to := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	site, err := m.SetMain(ctx, from, to)
	if err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	if site.Domain != from {
		if s.publisher != nil {
			s.publisher.Renamed(from, site.Domain)
		}
		if s.opts.Auth != nil {
			_ = s.opts.Auth.RenameSite(from, site.Domain)
		}
	}
	return site, nil
}

func (s *Server) sitesRemoveName(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	site, err := m.RemoveName(domain, r.PathValue("name"))
	if errors.Is(err, sites.ErrNotFound) {
		return nil, Errorf(http.StatusNotFound, "%v", err)
	}
	if err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return site, nil
}

func (s *Server) sitesDownload(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	dir := m.Dir(domain)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, Errorf(http.StatusNotFound, "%s: no such site", domain)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+domain+`.zip"`)
	// Streamed: a site can be gigabytes, and the zip is written as it is
	// read. An error halfway can only cut the stream, which the client
	// sees as a broken download rather than a wrong one.
	if _, err := archive.Zip(r.Context(), w, dir, isTrue(r.URL.Query().Get("all"))); err != nil {
		s.opts.Bus.Emit(events.Event{Type: "download.failed", Subject: domain, Actor: "operator", Level: "warn", Message: err.Error()})
	}
	return Handled, nil
}

// RegisterShots adds the site pictures.
func (s *Server) RegisterShots(sh *shots.Shotter) {
	p := Prefix + "/sites"
	s.Handle("GET", p+"/{domain}/shot.png", "The site's picture; asking for it keeps pictures renewing for an hour", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		sh.Viewed()
		path, at, ok := sh.Path(domain)
		if !ok || time.Since(at) > shots.Refresh {
			sh.Request(domain, false)
		}
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no picture of %s yet", domain)
		}
		w.Header().Set("Cache-Control", "private, max-age=60")
		http.ServeFile(w, r, path)
		return Handled, nil
	})
	s.Handle("POST", p+"/{domain}/shot", "Take the site's picture now, whatever state it is in", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !sh.Available() {
			return nil, Errorf(http.StatusNotImplemented, "%s", shots.NoBrowser)
		}
		sh.Request(domain, true)
		return OK, nil
	})
}
