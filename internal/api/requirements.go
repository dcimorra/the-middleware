package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/installer"
)

// The house's requirements, installed from Sistema (the helper's
// requirements.install). The run is in a unit of its own and writes its output
// to requirements.log in the helper's runtime directory; this reads that file, whose last line says how it
// ended, so the panel can follow it across reloads and across a restart of
// this process.

// requirementsStale is how long a log without its last line counts as running:
// longer, and the run was cut (a reboot, a kill) and is said as such.
const requirementsStale = 30 * time.Minute

// RequirementsStep is one line of the run's report. Optional is a step
// that failed without stopping the run (its line starts with "!"): the
// house works without it, and it is still not what was asked.
type RequirementsStep struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Optional bool   `json:"optional,omitempty"`
	Detail   string `json:"detail"`
}

// RequirementsStatus is what GET /system/requirements answers.
type RequirementsStatus struct {
	// State: none, running, finished, restart (finished, and the house must
	// restart to see docker's group), failed, interrupted.
	State string `json:"state"`
	// Steps are the run's ✓ and ✗ lines, read from the log: what the
	// panel checks a run by on every node at once without reading logs.
	Steps   []RequirementsStep `json:"steps"`
	Log     string             `json:"log"`
	Error   string             `json:"error,omitempty"`
	Updated time.Time          `json:"updated,omitzero"`
}

func (s *Server) requirementsStatus(path string) RequirementsStatus {
	info, err := os.Stat(path)
	if err != nil {
		return RequirementsStatus{State: "none"}
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	if len(text) > 256<<10 {
		text = "[…]\n" + text[len(text)-256<<10:]
	}
	st := RequirementsStatus{Log: text, Updated: info.ModTime(), State: "running", Steps: stepsOf(text)}
	last := ""
	if lines := strings.Split(strings.TrimSpace(text), "\n"); len(lines) > 0 {
		last = strings.TrimSpace(lines[len(lines)-1])
	}
	switch {
	case strings.HasPrefix(last, installer.PanelRestart) && s.opts.Started.After(info.ModTime()):
		// Asked to restart, and this process started after the run ended:
		// the restart happened, and the group reaches it now.
		st.State = "finished"
	case strings.HasPrefix(last, installer.PanelRestart):
		st.State = "restart"
	case strings.HasPrefix(last, installer.PanelFinished):
		st.State = "finished"
	case strings.HasPrefix(last, installer.PanelFailed):
		st.State, st.Error = "failed", strings.TrimPrefix(last, installer.PanelFailed)
	case time.Since(info.ModTime()) > requirementsStale:
		st.State, st.Error = "interrupted", "it stopped writing without saying how it ended"
	}
	return st
}

// RegisterRequirements mounts /system/requirements; run asks the helper to
// start it, and log is where the run writes (helper.InstallLog).
func (s *Server) RegisterRequirements(log string, run func(ctx context.Context, browser bool) error) {
	s.Handle("GET", Prefix+"/system/requirements", "The house's dependencies installed from the panel: state (none, running, finished, restart, failed, interrupted) and the installer's output", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.requirementsStatus(log), nil
	})
	s.Handle("POST", Prefix+"/system/requirements", "Install the house's dependencies (body: browser?): packages, docker and its group, nvm with the LTS, fail2ban and, with browser, a headless Chromium, through the helper in a unit of its own; nothing else of the machine is touched", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator installs the house's dependencies")
		}
		var body struct {
			Browser bool `json:"browser"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		if st := s.requirementsStatus(log); st.State == "running" {
			return nil, Errorf(http.StatusConflict, "the dependencies are already being installed")
		}
		// The last run's log goes first, so what is read from now on is
		// this run's and never says the last one's ending. In production
		// the file is root's and the helper removes it; this is for a
		// log the supervisor can.
		_ = os.Remove(log)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := run(ctx, body.Browser); err != nil {
			return nil, Errorf(http.StatusBadGateway, "the helper: %v", err)
		}
		return RequirementsStatus{State: "running"}, nil
	})
}

// stepsOf reads an installer's report lines ("✓ name  detail", "✗" a
// failed step, "!" an optional one that failed; the name
// padded to its column; installer.Check.String and Install print them).
// A name too long for the column has no padding to split at and is kept
// whole.
func stepsOf(log string) []RequirementsStep {
	out := []RequirementsStep{}
	for _, line := range strings.Split(log, "\n") {
		mark, rest, found := strings.Cut(line, " ")
		if !found || (mark != "✓" && mark != "✗" && mark != "!") {
			continue
		}
		name, detail := rest, ""
		if len(rest) > installer.NameColumn && rest[installer.NameColumn] == ' ' {
			name, detail = rest[:installer.NameColumn], rest[installer.NameColumn:]
		}
		out = append(out, RequirementsStep{Name: strings.TrimSpace(name), OK: mark == "✓", Optional: mark == "!", Detail: strings.TrimSpace(detail)})
	}
	return out
}
