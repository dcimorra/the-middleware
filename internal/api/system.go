package api

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/settings"
)

func (s *Server) registerSystem() {
	s.Handle("GET", Prefix+"/system", "Version, uptime, load, memory, disk", s.system)
	s.Handle("GET", Prefix+"/settings", "The installation's settings", s.settingsGet)
	s.Handle("PUT", Prefix+"/settings", "Replace the settings", s.settingsPut)
	s.Handle("POST", Prefix+"/system/restart", "Restart the supervisor (SIGTERM to self; systemd brings it back, sites keep running)", s.systemRestart)
	s.Handle("GET", Prefix+"/system/ports", "The site port range and what holds each port", s.systemPorts)
	s.Handle("GET", Prefix+"/system/processes", "Every process started for a site, with memory and CPU", s.systemProcesses)
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) (any, error) {
	st := s.opts.Settings()
	out := map[string]any{
		"version":      s.opts.Version,
		"root":         s.opts.Root,
		"uptime":       time.Since(s.opts.Started).Truncate(time.Second).String(),
		"started":      s.opts.Started,
		"listen":       s.opts.Listen,
		"panel_domain": st.PanelDomain,
		"go":           goruntime.Version(),
		"load":         loadAvg(),
		"memory":       memInfo(),
		"disk":         diskInfo(s.opts.Root),
		"hostname":     hostname(),
	}
	return out, nil
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.opts.Settings(), nil
}

func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request) (any, error) {
	if s.opts.SaveSettings == nil {
		return nil, Errorf(http.StatusNotImplemented, "settings cannot be changed here")
	}
	var st settings.Settings
	if err := ReadJSON(r, &st); err != nil {
		return nil, err
	}
	if st.Ports.From != 0 || st.Ports.To != 0 {
		if st.Ports.From < 1024 || st.Ports.To > 65535 || st.Ports.From >= st.Ports.To {
			return nil, Errorf(http.StatusBadRequest, "ports: want 1024 <= from < to <= 65535")
		}
	}
	// The proxy lowercases the request's host and compares it whole, so a
	// name saved as typed, "Panel.example.com" or with its root dot, was
	// a panel no request matched: the operator was locked out the moment
	// it was saved. The CLI's `domain` already normalises it this way.
	if st.PanelDomain != "" {
		d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(st.PanelDomain), "."))
		if !manifest.ValidDomain(d) {
			return nil, Errorf(http.StatusBadRequest, "panel_domain: %q is not a domain name", st.PanelDomain)
		}
		st.PanelDomain = d
	}
	// A body without the agents section (an older client) keeps the
	// defaults; one with it is held to the rules before anything is saved.
	st.Fill()
	st.Listen = strings.TrimSpace(st.Listen)
	if err := settings.ValidListen(st.Listen); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := s.canListen(st.Listen); err != nil {
		return nil, Errorf(http.StatusBadRequest, "listen: %s cannot be bound now: %v", st.Listen, err)
	}
	if !pages.IsTheme(st.VisitorTheme) {
		return nil, Errorf(http.StatusBadRequest, "visitor_theme: %q is not one of the designs", st.VisitorTheme)
	}
	if err := st.Agents.Validate(); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := st.Update.Validate(); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := st.Sites.Validate(); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := st.Autopilot.Validate(); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := s.opts.SaveSettings(st); err != nil {
		return nil, err
	}
	s.mu.Lock()
	fns := make([]func(settings.Settings), len(s.onSettings))
	copy(fns, s.onSettings)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(st)
	}
	s.opts.Bus.Emit(events.Event{Type: "settings.updated", Actor: "operator", Message: "settings replaced"})
	return s.opts.Settings(), nil
}

// canListen binds addr and lets it go, when addr is a change: a port
// another program holds, or an address this machine does not have, is
// found while the operator is looking at the form rather than at the next
// restart, when the supervisor would fail to start in a loop. The address
// in force is not tried, since this process holds it and the answer would
// always be no; neither is the one already saved, waiting for a restart.
func (s *Server) canListen(addr string) error {
	if addr == s.opts.Listen {
		return nil
	}
	if s.opts.Settings != nil {
		cur := s.opts.Settings()
		cur.Fill()
		if addr == cur.Listen {
			return nil
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

func (s *Server) systemRestart(w http.ResponseWriter, r *http.Request) (any, error) {
	s.opts.Bus.Emit(events.Event{Type: "middleware.restart", Actor: "operator", Message: "restart requested through the api"})
	restart := s.opts.Restart
	if restart == nil {
		restart = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
	}
	// After the answer has gone out, not before: the caller must see
	// its {ok} rather than a reset connection.
	time.AfterFunc(200*time.Millisecond, restart)
	return OK, nil
}

func (s *Server) systemPorts(w http.ResponseWriter, r *http.Request) (any, error) {
	st := s.opts.Settings()
	inUse := map[string]string{}
	if s.opts.Engine != nil {
		for _, sn := range s.opts.Engine.Sites() {
			if sn.Port != 0 {
				inUse[strconv.Itoa(sn.Port)] = sn.Domain
			}
			for name, wk := range sn.Workers {
				if wk.Port != 0 {
					inUse[strconv.Itoa(wk.Port)] = sn.Domain + "/" + name
				}
			}
		}
	}
	return map[string]any{"from": st.Ports.From, "to": st.Ports.To, "in_use": inUse}, nil
}

// Process is one entry of /system/processes.
type Process struct {
	PID     int       `json:"pid"`
	Domain  string    `json:"domain"`
	Kind    string    `json:"kind"` // "instance", "worker:<name>", "child"
	Port    int       `json:"port"`
	RSS     int64     `json:"rss"` // bytes
	CPU     float64   `json:"cpu"` // seconds of CPU consumed
	Started time.Time `json:"started"`
	Command string    `json:"command"`
}

// systemProcesses finds site processes by what the runtime put in their
// environment rather than by the pids the engine recorded: a worker's pid
// is not recorded, `npm start` is three processes, and a site adopted
// from a previous supervisor has no parent to ask. Reading another
// user's environ fails and that process is skipped, which is right: it
// is not ours.
func (s *Server) systemProcesses(w http.ResponseWriter, r *http.Request) (any, error) {
	ports := map[string]map[int]string{} // domain → port → kind
	if s.opts.Engine != nil {
		for _, sn := range s.opts.Engine.Sites() {
			ports[sn.Domain] = map[int]string{}
			if sn.Port != 0 {
				ports[sn.Domain][sn.Port] = "instance"
			}
			for name, wk := range sn.Workers {
				ports[sn.Domain][wk.Port] = "worker:" + name
			}
		}
	}
	return scanProcesses(ports), nil
}

func scanProcesses(ports map[string]map[int]string) []Process {
	out := []Process{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	boot := bootTime()
	hz := float64(100) // CLK_TCK on every Linux distribution that ships Go
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "environ"))
		if err != nil {
			continue
		}
		domain, port := "", 0
		for _, kv := range strings.Split(string(environ), "\x00") {
			if v, ok := strings.CutPrefix(kv, "MIDDLEWARE_DOMAIN="); ok {
				domain = v
			} else if v, ok := strings.CutPrefix(kv, "PORT="); ok {
				port, _ = strconv.Atoi(v)
			}
		}
		if domain == "" {
			continue
		}
		p := Process{PID: pid, Domain: domain, Port: port, Kind: "child"}
		if kinds, ok := ports[domain]; ok {
			if k, ok := kinds[port]; ok {
				p.Kind = k
			}
		}
		if stat, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "stat")); err == nil {
			// Fields after the parenthesised command name; the name may
			// contain spaces, hence the split at the last ')'.
			i := strings.LastIndexByte(string(stat), ')')
			if i > 0 {
				p.Command = string(stat[strings.IndexByte(string(stat), '(')+1 : i])
				fields := strings.Fields(string(stat[i+1:]))
				// fields[0] is state (field 3); utime is field 14, stime 15,
				// starttime 22 → indexes 11, 12, 19.
				if len(fields) > 19 {
					ut, _ := strconv.ParseFloat(fields[11], 64)
					stt, _ := strconv.ParseFloat(fields[12], 64)
					p.CPU = (ut + stt) / hz
					if start, err := strconv.ParseFloat(fields[19], 64); err == nil && !boot.IsZero() {
						p.Started = boot.Add(time.Duration(start / hz * float64(time.Second)))
					}
				}
			}
		}
		if statm, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "statm")); err == nil {
			fields := strings.Fields(string(statm))
			if len(fields) > 1 {
				pages, _ := strconv.ParseInt(fields[1], 10, 64)
				p.RSS = pages * int64(os.Getpagesize())
			}
		}
		if cmdline, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "cmdline")); err == nil && len(cmdline) > 0 {
			p.Command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func bootTime() time.Time {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err == nil {
				return time.Unix(secs, 0)
			}
		}
	}
	return time.Time{}
}

func loadAvg() map[string]float64 {
	out := map[string]float64{}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out
	}
	fields := strings.Fields(string(data))
	for i, key := range []string{"1m", "5m", "15m"} {
		if i < len(fields) {
			out[key], _ = strconv.ParseFloat(fields[i], 64)
		}
	}
	return out
}

func memInfo() map[string]int64 {
	out := map[string]int64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			out["total"] = kb * 1024
		case "MemAvailable":
			out["available"] = kb * 1024
		case "SwapTotal":
			out["swap_total"] = kb * 1024
		case "SwapFree":
			out["swap_free"] = kb * 1024
		}
	}
	if t, ok := out["total"]; ok {
		out["used"] = t - out["available"]
	}
	return out
}

func diskInfo(root string) map[string]int64 {
	out := map[string]int64{}
	if root == "" {
		root = "/"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		if err := syscall.Statfs("/", &st); err != nil {
			return out
		}
	}
	bs := int64(st.Bsize)
	out["total"] = int64(st.Blocks) * bs
	out["free"] = int64(st.Bavail) * bs
	out["used"] = out["total"] - int64(st.Bfree)*bs
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
