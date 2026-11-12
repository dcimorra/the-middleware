package relocate

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pubports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A worker that keeps its data in a named volume leaves it behind when its
// site moves: the target's migration fills a volume of the same name
// there, and the one here stays. Nothing took it out, so moving the site
// back here found it at the import, with the site already stopped there
// and its last copy sent: "the volume … is already on this node". Now the
// target says so at the check, before anything moves, and the source
// takes its own out once that is safe, or lists it for the operator.

// MoveVolumes are the docker volumes a move of domain fills on the target,
// for the workers named: the named volumes of those whose data moves by
// command.
func MoveVolumes(domain string, m *manifest.Manifest, workers []string) []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, name := range workers {
		w := m.Workers[name]
		if w == nil || w.Move == nil || w.Docker == nil {
			continue
		}
		for _, v := range w.Docker.Volumes {
			src, err := manifest.VolumeSource(v)
			if err != nil || !src.Named || src.Name == "" {
				continue
			}
			out = append(out, runtime.NamedVolume(domain, src.Name))
		}
	}
	sort.Strings(out)
	return out
}

// FixedPorts are the ports the site's workers have written in its
// manifest, which must be free wherever it runs: one taken there is a
// worker that does not start, found after the site stopped here.
func FixedPorts(m *manifest.Manifest) []int {
	if m == nil {
		return nil
	}
	var out []int
	for _, w := range m.Workers {
		if w != nil && !w.Disabled && w.Port != 0 {
			out = append(out, w.Port)
		}
	}
	sort.Ints(out)
	return out
}

// CheckNeeds says this node has none of the volumes the move would fill
// and every written port free. A volume here is data of an earlier life
// of the name, or of an attempt that was not cleaned: importing over it
// would mix the two, and removing it is not a move's decision.
func (r Receiver) CheckNeeds(ctx context.Context, need Needs) error {
	d := r.docker()
	for _, vol := range need.Volumes {
		if d.VolumeExists(ctx, vol) {
			return fmt.Errorf("the volume %s is already on this node (an earlier attempt, a site that had this name, or this site before it moved away): remove it here (docker volume rm %s, or from the leftovers of moves where it is listed) and move again", vol, vol)
		}
	}
	for _, port := range need.Ports {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return fmt.Errorf("port %d, which a worker of the site has written in its manifest, is taken on this node: free it here, or take the port out of the manifest (one is then given to the worker)", port)
		}
		l.Close()
	}
	public, err := pubports.Parse(need.Public)
	if err != nil {
		return err
	}
	if bound := pubports.Bound(public); len(bound) > 0 {
		return fmt.Errorf("%s, which a service of the site answers on, is in use on this node by something that is not the house's: free it here before moving the site", strings.Join(bound, ", "))
	}
	return nil
}

// Volumer is a Source whose workers' volumes are docker volumes of this
// machine, which it can take out.
type Volumer interface {
	RemoveVolumes(ctx context.Context, vols []string) error
}

// DockerVolumes is a Volumer on this machine's docker.
type DockerVolumes struct{ Docker runtime.Docker }

// RemoveVolumes implements Volumer.
func (v DockerVolumes) RemoveVolumes(ctx context.Context, vols []string) error {
	var errs []string
	for _, vol := range vols {
		if err := v.Docker.RemoveVolume(ctx, vol); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// sourceVolumes takes out, once a move is done, the volumes of the
// workers whose data it moved by command, or lists them as leftovers.
//
// Only once the copy here was archived, and only for the workers exported
// with the site stopped whose dump is in that archive: their volume then
// holds nothing that is not both in the database there and in the archive
// here. A live worker's volume is kept, listed: what the site wrote after
// its export is in that volume and nowhere else (it did not travel, as
// "live" says), and only the operator can say it is worth nothing. A
// volume whose worker's dump is not in the folder when it is packed, or a
// move whose archive was not written, keeps them all.
func (m *Mover) sourceVolumes(ctx context.Context, domain string, mf *manifest.Manifest, archived string, dumped map[string]bool) {
	v, ok := m.Source.(Volumer)
	if !ok || mf == nil {
		return
	}
	var safe, kept []string
	for _, name := range MovingWorkers(mf) {
		vols := MoveVolumes(domain, mf, []string{name})
		if archived != "" && !mf.Workers[name].Move.Live && dumped[name] {
			safe = append(safe, vols...)
		} else {
			kept = append(kept, vols...)
		}
	}
	if len(safe) > 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		if err := v.RemoveVolumes(rctx, safe); err != nil {
			kept = append(kept, safe...)
		}
		cancel()
	}
	if len(kept) > 0 {
		_ = LeaveVolumes(m.Root, domain, kept)
	}
}

// dumpsThere says, for each worker whose data moves by command, that its
// dump is in dir: its output is there, or it has none (its export writes
// its own files, which the house cannot name).
func dumpsThere(dir string, mf *manifest.Manifest) map[string]bool {
	out := map[string]bool{}
	if mf == nil {
		return out
	}
	for _, name := range MovingWorkers(mf) {
		o := mf.Workers[name].Move.Output
		out[name] = o == "" || fsx.Exists(filepath.Join(dir, filepath.FromSlash(o)))
	}
	return out
}

var volumesMu sync.Mutex

func volumesPath(root string) string {
	return filepath.Join(root, "data", "relocate", ".volumes.json")
}

// leftVolume is a docker volume a move left on this node, by site.
type leftVolume struct {
	Domain string    `json:"domain"`
	At     time.Time `json:"at"`
}

// LeaveVolumes writes down that the moves of domain left vols here, for
// Leftovers to list.
func LeaveVolumes(root, domain string, vols []string) error {
	volumesMu.Lock()
	defer volumesMu.Unlock()
	rec := map[string]leftVolume{}
	_ = fsx.ReadJSON(volumesPath(root), &rec)
	for _, v := range vols {
		rec[v] = leftVolume{Domain: domain, At: time.Now().UTC()}
	}
	if err := os.MkdirAll(filepath.Dir(volumesPath(root)), 0o755); err != nil {
		return err
	}
	return fsx.WriteJSON(volumesPath(root), rec, 0o644)
}

func leftVolumes(root string) map[string]leftVolume {
	volumesMu.Lock()
	defer volumesMu.Unlock()
	rec := map[string]leftVolume{}
	_ = fsx.ReadJSON(volumesPath(root), &rec)
	return rec
}

// forgetVolume removes vol from what moves left.
func forgetVolume(root, vol string) error {
	volumesMu.Lock()
	defer volumesMu.Unlock()
	rec := map[string]leftVolume{}
	_ = fsx.ReadJSON(volumesPath(root), &rec)
	delete(rec, vol)
	return fsx.WriteJSON(volumesPath(root), rec, 0o644)
}

// volumePrefix is how a volume is named among the leftovers' paths,
// which are otherwise files under the root.
const volumePrefix = "docker-volume:"
