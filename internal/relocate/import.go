package relocate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/migrate"
)

// Importing a site from a V1 house into a V2 one is a move whose source is
// not a node of the network: the V2 binary, run on the V1 machine, packs
// the site's folder and sends it to the V2 house over HTTPS with a token
// that house made for this one site and two hours (the import scope). The
// V1 site is stopped the way V1 stops a site, by writing disabled in its
// manifest, and started again the same way if the V2 copy does not come
// up. What V1 keeps of the site under its data/ (conversations, cost) is
// written in V1's own formats and stays there; the folder and its HOME go.

// V2ManifestName is the V2 manifest a V1 site may carry next to its own.
const V2ManifestName = "site.v2.json"

// HTTPTarget is a V2 house reached at its panel's address with a token.
type HTTPTarget struct {
	// Base is the house's address, "https://panel.example.com".
	Base  string
	Token string
	// Label names it in the messages; empty is Base.
	Label  string
	Client *http.Client
	// Install asks the house to install the site's dependencies before
	// putting it in place: they are not sent (node_modules is built for
	// the machine it was installed on).
	Install bool
}

// Name implements Target.
func (t HTTPTarget) Name() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Base
}

func (t HTTPTarget) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{}
}

func (t HTTPTarget) do(ctx context.Context, method, path, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.Base, "/")+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := t.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		if res.StatusCode == http.StatusRequestEntityTooLarge {
			msg += " (the bundle is larger than what the way in lets through: Cloudflare's tunnel takes 100 MB; reach the house by its ports door or the VPN)"
		}
		return fmt.Errorf("%s answered %d: %s", t.Name(), res.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func jsonBody(v any) io.Reader {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw)
}

// Prepare implements Preparer: the house installs the site's dependencies
// on its own and is asked how it goes, so the install is not inside one
// long request (which a Cloudflare tunnel cuts at 100 seconds, and the
// install with it). A house too old to have the route installs at the
// commit, as before.
func (t HTTPTarget) Prepare(ctx context.Context, d string, dirs []string) (string, error) {
	if !t.Install {
		return "", nil
	}
	id, err := t.startJob(ctx, "/relocate/"+d+"/prepare", "install", d, map[string]any{"dirs": dirs})
	if err != nil {
		if strings.Contains(err.Error(), "answered 404") || strings.Contains(err.Error(), "answered 403") {
			return "", nil
		}
		return "", err
	}
	return t.waitJob(ctx, "/relocate/"+d+"/prepare", "install of "+d, id)
}

// Migrate implements Migrator: the house runs the workers' imports on its
// own and is asked how it goes, as Prepare. A house too old to have the
// route is an error, not a skip: the site would start there on an empty
// database.
func (t HTTPTarget) Migrate(ctx context.Context, d string, manifestData []byte, workers []string) (string, error) {
	body := map[string]any{"manifest": base64.StdEncoding.EncodeToString(manifestData), "workers": workers}
	id, err := t.startJob(ctx, "/relocate/"+d+"/migrate", "data import", d, body)
	if err != nil {
		if strings.Contains(err.Error(), "answered 404") {
			return "", fmt.Errorf("%s cannot import a worker's data (update it to a version that can)", t.Name())
		}
		return "", err
	}
	return t.waitJob(ctx, "/relocate/"+d+"/migrate", "data import of "+d, id)
}

// startJob posts to path and says the job it started there; one already
// running is an earlier attempt's, on a staging received again since,
// and what it ends with is not this import's.
func (t HTTPTarget) startJob(ctx context.Context, path, what, d string, body any) (string, error) {
	var st struct {
		Started bool   `json:"started"`
		Running bool   `json:"running"`
		ID      string `json:"id"`
	}
	if err := t.do(ctx, "POST", path, "application/json", jsonBody(body), &st); err != nil {
		return "", err
	}
	if !st.Started && st.Running {
		return "", fmt.Errorf("an earlier %s of %s is still running on %s: import it again once that one ends", what, d, t.Name())
	}
	return st.ID, nil
}

// jobPoll is how often a job on the house is asked about.
var jobPoll = 3 * time.Second

// waitJob asks path until the job id ends there. A question unanswered is
// asked again; a house that answers 404 has forgotten the job (it
// restarted, and its jobs live in its memory) and would say the same until
// the import's time is out, with the site stopped here all that while.
func (t HTTPTarget) waitJob(ctx context.Context, path, what, id string) (string, error) {
	for {
		var j struct {
			ID     string `json:"id"`
			Done   bool   `json:"done"`
			Output string `json:"output"`
			Error  string `json:"error"`
		}
		err := t.do(ctx, "GET", path, "", nil, &j)
		switch {
		case err != nil && strings.Contains(err.Error(), "answered 404"):
			return "", fmt.Errorf("%s no longer knows of the %s (it restarted while it ran): it will never end there", t.Name(), what)
		case err != nil:
		case id != "" && j.ID != id:
			return "", fmt.Errorf("%s answers for another %s than the one this import started (it restarted, and something started one since)", t.Name(), what)
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

// Check implements Target.
func (t HTTPTarget) Check(ctx context.Context, d string, need Needs) error {
	err := t.do(ctx, "POST", "/relocate/"+d+"/check", "application/json", jsonBody(need), nil)
	if err != nil && strings.Contains(err.Error(), "answered 400") && strings.Contains(err.Error(), "unknown field") {
		// An older house reads the check strictly, and what it does not
		// know (the volumes and ports asked about now) refuses it whole.
		return fmt.Errorf("%s runs an older version that cannot check what this site needs: update it first (%v)", t.Name(), err)
	}
	return err
}

// Send implements Target.
func (t HTTPTarget) Send(ctx context.Context, d string, r io.Reader) error {
	// carries: an import has no house on the other side whose volumes
	// could be left behind; what its workers keep moves by their commands.
	return t.do(ctx, "PUT", "/relocate/"+d+"?carries=1", "application/gzip", r, nil)
}

// SendDelta implements Target.
func (t HTTPTarget) SendDelta(ctx context.Context, d string, r io.Reader) error {
	return t.do(ctx, "PUT", "/relocate/"+d+"/delta", "application/gzip", r, nil)
}

// Commit implements Target.
func (t HTTPTarget) Commit(ctx context.Context, d string, m []byte, taskFiles map[string][]byte) error {
	body := map[string]any{"manifest": base64.StdEncoding.EncodeToString(m), "install": t.Install}
	if len(taskFiles) > 0 {
		body["tasks"] = EncodeTasks(taskFiles)
	}
	return t.do(ctx, "POST", "/relocate/"+d+"/commit", "application/json", jsonBody(body), nil)
}

// EncodeTasks is how task files travel in a commit's body, by name.
func EncodeTasks(taskFiles map[string][]byte) map[string]string {
	out := make(map[string]string, len(taskFiles))
	for name, data := range taskFiles {
		out[name] = base64.StdEncoding.EncodeToString(data)
	}
	return out
}

// DecodeTasks is EncodeTasks undone, for the receiving side.
func DecodeTasks(in map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(in))
	for name, s := range in {
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("task %s: not base64", name)
		}
		out[name] = data
	}
	return out, nil
}

// Discard implements Target.
func (t HTTPTarget) Discard(ctx context.Context, d string) error {
	return t.do(ctx, "DELETE", "/relocate/"+d, "", nil, nil)
}

// Repair implements Repairer: the house's AI there looks at the site
// before the import is undone. A house with none, or too old to have the
// route, says so, and the import is undone as before.
func (t HTTPTarget) Repair(ctx context.Context, d, why string) (string, error) {
	if err := t.do(ctx, "POST", "/relocate/"+d+"/repair", "application/json", jsonBody(map[string]string{"why": why}), nil); err != nil {
		if strings.Contains(err.Error(), "answered 409") || strings.Contains(err.Error(), "answered 404") || strings.Contains(err.Error(), "answered 403") {
			return "", ErrNoRepair
		}
		return "", err
	}
	return t.waitJob(ctx, "/relocate/"+d+"/repair", "repair of "+d, "")
}

// Finish implements Finisher.
func (t HTTPTarget) Finish(ctx context.Context, d string) error {
	return t.do(ctx, "POST", "/relocate/"+d+"/placed", "", nil, nil)
}

// Remove implements Target.
func (t HTTPTarget) Remove(ctx context.Context, d string) error {
	return t.do(ctx, "DELETE", "/sites/"+d, "", nil, nil)
}

// Ready implements Target: the same rule as between nodes, read from the
// site as the house describes it.
func (t HTTPTarget) Ready(ctx context.Context, d string) (bool, string, error) {
	var s struct {
		Alive   bool   `json:"alive"`
		Error   string `json:"error"`
		Mode    string `json:"mode"`
		Workers map[string]struct {
			Alive bool `json:"alive"`
		} `json:"workers"`
	}
	if err := t.do(ctx, "GET", "/sites/"+d, "", nil, &s); err != nil {
		return false, "", err
	}
	switch {
	case s.Error != "":
		return false, s.Error, nil
	case s.Alive, s.Mode == "static":
		return true, "", nil
	case s.Mode == "project" && len(s.Workers) > 0:
		for name, w := range s.Workers {
			if !w.Alive {
				return false, "worker " + name + " is not up", nil
			}
		}
		return true, "", nil
	}
	return false, "not up yet", nil
}

// Publish implements Target. An import does not move the names: the V1
// house still has them, and taking them is the operator's yes in the V2
// panel (Publish, then "point it here" for each name the zone has on V1).
func (t HTTPTarget) Publish(ctx context.Context, d, door string) error {
	// over: the names point at the machine the site came from, and follow
	// it here, as a move between nodes does. A house too old to let the
	// import token do it says so, and the names stay for the panel.
	return t.do(ctx, "POST", "/sites/"+d+"/publish", "application/json", jsonBody(map[string]any{"door": door, "over": true}), nil)
}

// ImportParts are what an import takes: the folder, without what the
// target installs again, and the site's HOME.
func ImportParts(root, domain string) []archive.Part {
	skip := SkipReinstalled
	return []archive.Part{
		{Name: "site", Path: filepath.Join(root, "sites", domain), Skip: skip},
		{Name: "data/homes/" + domain, Path: filepath.Join(root, "data", "homes", domain), Skip: skip},
	}
}

// V1Source is a V1 house's site as the source of an import.
type V1Source struct {
	// Root is the V1 house's root (the folder sites/ is in).
	Root string
	// Wait is how long the V1 engine gets to stop the site.
	Wait time.Duration
	// Running says a process still works in dir; replaced by tests.
	Running func(dir string) bool
	// TasksOff has the site's tasks arrive turned off (import --tasks-off):
	// a site that stays on the target only for a while, on a machine too
	// small for its nightly jobs, keeps them for when it moves on.
	TasksOff bool

	original  []byte
	converted []byte
	docker    bool
	heldTasks map[string][]byte
}

// Convert reads and converts the V1 manifest, before anything moves: a
// site V2 cannot honour yet is refused here.
func (s *V1Source) Convert(domain string) (notes []string, err error) {
	dir := filepath.Join(s.Root, "sites", domain)
	raw, err := os.ReadFile(filepath.Join(dir, "site.json"))
	if err != nil {
		return nil, err
	}
	// site.v2.json, next to the V1 one, is the manifest the site will have
	// in V2, written by a person: what V1 has no words for (a database in
	// a container, how its data moves) is said there, and the conversion
	// is not asked to guess it.
	if v2, err := os.ReadFile(filepath.Join(dir, V2ManifestName)); err == nil {
		m, err := manifest.Parse(v2)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", V2ManifestName, err)
		}
		if err := m.CheckInside(dir); err != nil {
			return nil, fmt.Errorf("%s: %w", V2ManifestName, err)
		}
		m.Disabled = false
		out, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, err
		}
		s.original, s.converted = raw, append(out, '\n')
		s.docker = UsesDocker(m)
		notes := []string{"manifest: " + V2ManifestName + ", not converted from the V1 one"}
		for _, name := range m.MovingWorkers(true) {
			notes = append(notes, "worker "+name+": its data moves by command while it serves (export here and import there before it stops; what it writes after the export does not travel)")
		}
		for _, name := range m.MovingWorkers(false) {
			notes = append(notes, "worker "+name+": its data moves by command (export here with the site stopped, import there before it starts)")
		}
		return notes, nil
	}
	res := migrate.Convert(domain, raw)
	if res.Skipped != "" {
		return nil, errors.New(res.Skipped)
	}
	if !res.Ready() {
		return nil, fmt.Errorf("V2 cannot honour this site yet: %s", strings.Join(res.Blockers, "; "))
	}
	res.Manifest.Disabled = false
	out, err := json.MarshalIndent(res.Manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	s.original, s.converted = raw, append(out, '\n')
	s.docker = UsesDocker(res.Manifest)
	return res.Notes, nil
}

// Needs implements Needer. The mover adds the tasks (Tasker).
//
// Its names too, all of them: the converted manifest's, aliases and their
// www included. V1 said none, so the target was never asked whether it
// could point them, and a name it could not (homeify.cc, an alias in a
// zone its DNS provider does not have) was found out at the end, with the
// site already stopped here and the name left pointing at it.
func (s *V1Source) Needs(domain string) Needs {
	return Needs{Docker: s.docker, Names: s.Names(domain)}
}

// Stop writes disabled into the V1 manifest, which is how V1 stops a site,
// and waits for its processes to be gone. It returns the converted V2
// manifest: that is what the V2 house commits.
func (s *V1Source) Stop(ctx context.Context, domain string) ([]byte, error) {
	if s.converted == nil {
		if _, err := s.Convert(domain); err != nil {
			return nil, err
		}
	}
	var m map[string]any
	if err := json.Unmarshal(s.original, &m); err != nil {
		return nil, err
	}
	m["disabled"] = true
	raw, _ := json.MarshalIndent(m, "", "  ")
	dir := filepath.Join(s.Root, "sites", domain)
	if err := fsx.WriteAtomic(filepath.Join(dir, "site.json"), append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	wait := s.Wait
	if wait == 0 {
		wait = time.Minute
	}
	running := s.Running
	if running == nil {
		running = processIn
	}
	deadline := time.Now().Add(wait)
	for running(dir) {
		if time.Now().After(deadline) {
			_ = s.Start(domain, nil)
			return nil, fmt.Errorf("V1 did not stop %s in %s", domain, wait)
		}
		select {
		case <-ctx.Done():
			_ = s.Start(domain, nil)
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return s.converted, nil
}

// Start puts the V1 manifest back as it was, which starts the site in V1.
func (s *V1Source) Start(domain string, _ []byte) error {
	return fsx.WriteAtomic(filepath.Join(s.Root, "sites", domain, "site.json"), s.original, 0o644)
}

// Published implements Source: the names follow the site, on the target's
// door for new sites (empty), and are pointed there once it answers. The
// operator used to do it by hand, name by name ("Point it here").
//
// A project answers no name, and asking the target to publish it only
// ended the import with a warning that was not one.
func (s *V1Source) Published(domain string) (string, bool) { return "", !manifest.IsProject(domain) }

// Names implements Source.
func (s *V1Source) Names(domain string) []string {
	m, err := manifest.Parse(s.converted)
	if err != nil {
		return nil
	}
	return m.Hosts(domain)
}

// Unpublish implements Source.
func (s *V1Source) Unpublish(context.Context, string) {}

// Archive implements Source: the V1 site is left stopped, its folder
// where it was, for the V1 panel to archive once the V2 copy has served a
// while; going back is starting it there.
func (s *V1Source) Archive(context.Context, string, []byte) (string, error) { return "", nil }

// processIn says some process has its working directory in dir: what
// is left of a site V1 is stopping.
func processIn(dir string) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	for _, e := range ents {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		if cwd == real || strings.HasPrefix(cwd, real+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
