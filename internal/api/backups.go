package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dcimorra/the-middleware/internal/backups"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// RegisterBackups mounts a site's database copies. What is copied, when
// and how many are kept is the manifest's (backups), written as any part
// of it is; these routes are what is done with it: list, copy now,
// download, restore, delete.
//
// A copy and a restore take what the database takes, minutes for a large
// one: they run on their own and answer at once (progress as backup.*
// events), unless the caller asks to wait, as the CLI does for an agent
// that wants to know how it ended.
func (s *Server) RegisterBackups(k *backups.Keeper) {
	s.registerDatabases(k)
	base := Prefix + "/sites/{domain}/backups"
	s.Handle("GET", base, "The site's backups as its manifest describes them, each with its copies (newest first), next run, last copy and last restore", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		list, err := k.List(domain)
		return list, backupErr(err)
	})
	s.Handle("POST", base+"/{name}/run", "Take a copy now: {wait?} → 202 {started}, or with wait the copy {id, at, bytes, kind} once it is written", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := backupOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Wait bool `json:"wait"`
		}
		if err := optionalJSON(r, &body); err != nil {
			return nil, err
		}
		if body.Wait {
			c, err := k.Backup(r.Context(), domain, name, "manual")
			if err != nil {
				return nil, failedErr(err)
			}
			return c, nil
		}
		if err := k.Check(domain, name, ""); err != nil {
			return nil, backupErr(err)
		}
		// Not the request's context: a copy that takes ten minutes must
		// not stop because the tab that asked for it closed.
		go func() { _, _ = k.Backup(context.Background(), domain, name, "manual") }()
		WriteJSON(w, http.StatusAccepted, map[string]any{"started": true})
		return Handled, nil
	})
	s.Handle("GET", base+"/{name}/log", "The end of the backup's log: what its dumps and restores said. ?lines=N (200 by default)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := backupOf(r)
		if err != nil {
			return nil, err
		}
		lines := 200
		if v := r.URL.Query().Get("lines"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
				lines = n
			}
		}
		text, err := k.Log(domain, name, lines)
		if err != nil {
			return nil, backupErr(err)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
		return Handled, nil
	})
	s.Handle("GET", base+"/{name}/copies/{id}", "Download a copy, compressed as it is kept (gzip)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := backupOf(r)
		if err != nil {
			return nil, err
		}
		p, err := k.Path(domain, name, r.PathValue("id"))
		if err != nil {
			return nil, backupErr(err)
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		file := domain + "-" + name + "-" + r.PathValue("id")
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, file, info.ModTime(), f)
		return Handled, nil
	})
	s.Handle("DELETE", base+"/{name}/copies/{id}", "Delete a copy", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := backupOf(r)
		if err != nil {
			return nil, err
		}
		if err := k.Delete(domain, name, r.PathValue("id")); err != nil {
			return nil, backupErr(err)
		}
		return OK, nil
	})
	s.Handle("POST", base+"/{name}/copies/{id}/restore", "Put a copy back over the database: {wait?, no_safety_copy?}. A copy of the database as it is now is taken first (kind pre-restore) and nothing is restored if it fails, unless no_safety_copy. → 202 {started}, or with wait the result {ok, error?, seconds}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := backupOf(r)
		if err != nil {
			return nil, err
		}
		id := r.PathValue("id")
		var body struct {
			Wait         bool `json:"wait"`
			NoSafetyCopy bool `json:"no_safety_copy"`
		}
		if err := optionalJSON(r, &body); err != nil {
			return nil, err
		}
		o := backups.RestoreOptions{Trigger: "manual", NoSafetyCopy: body.NoSafetyCopy}
		if body.Wait {
			res, err := k.Restore(r.Context(), domain, name, id, o)
			if err != nil {
				return nil, failedErr(err)
			}
			return res, nil
		}
		if err := k.Check(domain, name, id); err != nil {
			return nil, backupErr(err)
		}
		go func() { _, _ = k.Restore(context.Background(), domain, name, id, o) }()
		WriteJSON(w, http.StatusAccepted, map[string]any{"started": true})
		return Handled, nil
	})
}

func backupOf(r *http.Request) (string, string, error) {
	domain, err := domainOf(r)
	if err != nil {
		return "", "", err
	}
	name := r.PathValue("name")
	if !manifest.ValidBackupName(name) {
		return "", "", Errorf(http.StatusBadRequest, "%q is not a backup's name", name)
	}
	return domain, name, nil
}

// optionalJSON reads a body that may be left out.
func optionalJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	err := ReadJSON(r, v)
	var e *Error
	if errors.As(err, &e) && strings.HasSuffix(e.Message, "EOF") {
		return nil
	}
	return err
}

func backupErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, backups.ErrNotFound):
		return Errorf(http.StatusNotFound, "%v", err)
	case errors.Is(err, backups.ErrBusy):
		return Errorf(http.StatusConflict, "%v", err)
	case errors.Is(err, backups.ErrInvalid), errors.Is(err, backups.ErrNoRestore):
		return Errorf(http.StatusBadRequest, "%v", err)
	}
	return err
}

// failedErr is a copy or a restore that ran and failed: the database's
// answer, not the house's fault, so not a 500 (which the house reports
// as its own error).
func failedErr(err error) error {
	if e := backupErr(err); e != err {
		return e
	}
	return Errorf(http.StatusUnprocessableEntity, "%v", err)
}
