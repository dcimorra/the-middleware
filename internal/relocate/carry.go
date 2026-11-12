package relocate

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A site's docker volumes go with it. A move carried the folder and what
// the house keeps under data/, and a service that kept its database in a
// named volume (verdelaurel.es: Postgres 17 in mw-verdelaurel-es-db)
// arrived to an empty one: initdb ran, the web's migrations made the
// tables again and seeded a demo catalogue, and the site answered healthy
// with nothing in it while every order it had taken stayed on the node it
// left. Only workers that said how their data moves (move.export) took
// it along.
//
// Now every volume that is the site's, and that no move command fills,
// is copied with the site stopped, file for file, and read back on the
// target: the move goes on only when what is on the target's disk is what
// was read here, name by name with owners, modes and contents. Anything
// less undoes the move before the site is put in place there, so nothing
// ever starts on a volume that did not arrive. The volume here is only
// ever read, and stays, listed among what moves left, until the operator
// removes it.

// StepVolumes is the copy of the site's docker volumes, with it stopped.
const StepVolumes = "volumes"

// VolumeSum is what a volume holds, as both ends of a move count it.
type VolumeSum struct {
	// Digest covers every name with its kind, mode, owner, size, link and
	// contents, in name order. Not times: a folder's changes as it is
	// filled, and nothing that reads a volume asks for them.
	Digest string `json:"digest"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// Carrier is a Source that can say which docker volumes are its site's
// and read them out.
type Carrier interface {
	// Volumes are the site's volumes on this node; an error is a docker
	// that could not be asked.
	Volumes(ctx context.Context, domain string) ([]string, error)
	// VolumeUsers are the running containers that mount one.
	VolumeUsers(ctx context.Context, volume string) ([]string, error)
	// ReadVolume writes one out as a tar (runtime.Docker.ReadVolume).
	ReadVolume(ctx context.Context, volume string, w io.Writer) error
}

// VolumeTaker is a target that takes a site's volume as a tar, writes it
// into a new volume of that name, reads it back and says what is there.
type VolumeTaker interface {
	TakeVolume(ctx context.Context, domain, volume string, body io.Reader) (VolumeSum, error)
}

// NamedVolumes are the docker volumes the manifest's site, workers and
// services mount by name, as docker calls them.
func NamedVolumes(domain string, m *manifest.Manifest) []string {
	if m == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(d *manifest.Docker) {
		if d == nil {
			return
		}
		for _, v := range d.Volumes {
			if src, err := manifest.VolumeSource(v); err == nil && src.Named && src.Name != "" {
				seen[runtime.NamedVolume(domain, src.Name)] = true
			}
		}
	}
	if m.Run != nil {
		add(m.Run.Docker)
	}
	for _, w := range m.Workers {
		if w != nil {
			add(w.Docker)
		}
	}
	for _, sv := range m.Services {
		if sv != nil && sv.Run != nil {
			add(sv.Run.Docker)
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Summarize reads a volume's tar to its end, copying every byte of it to
// out (nil for nowhere), and says what it holds. A name outside
// runtime.VolumeMount is an error: the tar is laid over a container's
// root on the other side.
func Summarize(r io.Reader, out io.Writer) (VolumeSum, error) {
	var sum VolumeSum
	if out == nil {
		out = io.Discard
	}
	tee := io.TeeReader(r, out)
	tr := tar.NewReader(tee)
	inside := func(name string) bool {
		for _, el := range strings.Split(name, "/") {
			if el == ".." {
				return false
			}
		}
		c := path.Clean(name)
		return c == runtime.VolumeMount || strings.HasPrefix(c, runtime.VolumeMount+"/")
	}
	var lines []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, fmt.Errorf("the volume's tar: %w", err)
		}
		if !inside(h.Name) || h.Typeflag == tar.TypeLink && !inside(h.Linkname) {
			return sum, fmt.Errorf("the volume's tar names %q, outside the volume", h.Name)
		}
		hash := sha256.New()
		n, err := io.Copy(hash, tr)
		if err != nil {
			return sum, fmt.Errorf("the volume's tar, at %s: %w", h.Name, err)
		}
		kind := h.Typeflag
		if kind == 0 {
			kind = tar.TypeReg
		}
		if kind == tar.TypeReg {
			sum.Files++
			sum.Bytes += n
		}
		lines = append(lines, fmt.Sprintf("%q %c %o %d %d %d %q %d %d %x", path.Clean(h.Name), kind, h.Mode&0o7777, h.Uid, h.Gid, n, h.Linkname, h.Devmajor, h.Devminor, hash.Sum(nil)))
	}
	// What follows the last entry (the end blocks, padding) goes too: the
	// other side's reader waits for it.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return sum, fmt.Errorf("the volume's tar: %w", err)
	}
	if len(lines) == 0 {
		return sum, errors.New("the volume's tar is empty: not even the volume's own folder is in it")
	}
	sort.Strings(lines)
	all := sha256.New()
	for _, l := range lines {
		io.WriteString(all, l)
		all.Write([]byte{'\n'})
	}
	sum.Digest = hex.EncodeToString(all.Sum(nil))
	return sum, nil
}

// carried are the volumes a move of domain copies as files: the site's
// on this node, less the ones its workers' move commands fill there.
func (m *Mover) carried(ctx context.Context, domain string, planned *manifest.Manifest, moving []string) ([]string, error) {
	c, ok := m.Source.(Carrier)
	if !ok {
		return nil, nil
	}
	vols, err := c.Volumes(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("asking docker which volumes are the site's: %w; a move that cannot ask would leave them behind", err)
	}
	byCommand := map[string]bool{}
	for _, v := range MoveVolumes(domain, planned, moving) {
		byCommand[v] = true
	}
	var out []string
	for _, v := range vols {
		if !byCommand[v] {
			out = append(out, v)
		}
	}
	return out, nil
}

// quiet waits for no running container to mount the volume: a stop that
// was answered is a container told to stop, and a database takes its
// seconds to close its files.
func quiet(ctx context.Context, c Carrier, volume string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		users, err := c.VolumeUsers(ctx, volume)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the volume %s is still mounted by a running container (%s) with the site stopped: copied now it would be a torn copy", volume, strings.Join(users, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// errStopped is what the volume's reader is told when nobody takes what
// it reads any more.
var errStopped = errors.New("stopped reading the volume")

// quietWait is how long quiet waits; the tests make it short.
var quietWait = 90 * time.Second

// carry copies each volume to t and compares what t read back from its
// disk with what was read here.
func (m *Mover) carry(ctx context.Context, domain string, t Target, vols []string, progress func(step, msg string)) error {
	c, taker := m.Source.(Carrier), t.(VolumeTaker)
	for _, vol := range vols {
		if err := quiet(ctx, c, vol, quietWait); err != nil {
			return err
		}
		pr, pw := io.Pipe()
		read := make(chan error, 1)
		go func() {
			err := c.ReadVolume(ctx, vol, pw)
			pw.CloseWithError(err)
			read <- err
		}()
		// Counted as it goes by, on its way to the target.
		body, bw := io.Pipe()
		var here VolumeSum
		summed := make(chan error, 1)
		go func() {
			var err error
			here, err = Summarize(pr, bw)
			// Whatever the reader still writes has somewhere to go.
			_ = pr.CloseWithError(errStopped)
			bw.CloseWithError(err)
			summed <- err
		}()
		there, err := taker.TakeVolume(ctx, domain, vol, body)
		_ = body.CloseWithError(errors.New("the target stopped reading"))
		sumErr, readErr := <-summed, <-read
		switch {
		// A read that was stopped is what follows the target's failure,
		// not the failure.
		case readErr != nil && !errors.Is(readErr, errStopped):
			return fmt.Errorf("the volume %s: %w", vol, readErr)
		case err != nil:
			return fmt.Errorf("the volume %s on %s: %w", vol, t.Name(), err)
		case sumErr != nil:
			return fmt.Errorf("the volume %s: %w", vol, sumErr)
		case there != here:
			return fmt.Errorf("the volume %s is not on %s as it is here (%d files and %d bytes there, %d and %d here, or the same with something in them different): the copy is not to be trusted", vol, t.Name(), there.Files, there.Bytes, here.Files, here.Bytes)
		}
		progress(StepVolumes, fmt.Sprintf("%s is on %s as it is here: %d files, %s, read back from its disk", vol, t.Name(), here.Files, sizeWords(here.Bytes)))
	}
	return nil
}

func sizeWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ErrVolumeHere is a volume a move brings that this node already has.
var ErrVolumeHere = errors.New("the volume is already on this node")

// TakeVolume makes volume here for the site being received and fills it
// from body, a tar as runtime.Docker.ReadVolume writes one. It answers
// with what it read back from the volume once written, not with what it
// was sent: what the mover compares is what is on this disk.
//
// The volume is written down before it is made, so a move that is undone
// takes it out (Discard, Unplace); one that fails here is removed at
// once, for a half-written volume under the site's name is exactly what
// a container must never find.
func (r Receiver) TakeVolume(ctx context.Context, domain, volume string, body io.Reader) (VolumeSum, error) {
	var none VolumeSum
	if err := r.check(domain); err != nil {
		return none, err
	}
	if !fsx.Exists(r.staging(domain)) {
		return none, ErrNoStaging
	}
	if !volumeNameRe.MatchString(volume) || !strings.HasPrefix(volume, runtime.NamedVolume(domain, "")) {
		return none, fmt.Errorf("%w: %q is not a volume of %s", ErrBadName, volume, domain)
	}
	d := r.docker()
	if d.VolumeExists(ctx, volume) {
		return none, fmt.Errorf("%w: %s (an earlier attempt, a site that had this name, or this site before it moved away): remove it here and move again", ErrVolumeHere, volume)
	}
	var listed []string
	if raw, err := os.ReadFile(r.volumesFile(domain)); err == nil {
		_ = json.Unmarshal(raw, &listed)
	}
	if err := fsx.WriteJSON(r.volumesFile(domain), append(listed, volume), 0o644); err != nil {
		return none, err
	}
	fail := func(err error) (VolumeSum, error) {
		rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if rerr := d.RemoveVolume(rctx, volume); rerr != nil {
			return none, fmt.Errorf("%w; and what was written of it could not be removed: %v", err, rerr)
		}
		return none, err
	}
	if err := d.ClaimVolume(ctx, domain, volume); err != nil {
		return fail(err)
	}
	pr, pw := io.Pipe()
	var sent VolumeSum
	summed := make(chan error, 1)
	go func() {
		var err error
		sent, err = Summarize(body, pw)
		pw.CloseWithError(err)
		summed <- err
	}()
	err := d.WriteVolume(ctx, volume, pr)
	_ = pr.CloseWithError(errors.New("docker stopped reading"))
	if sumErr := <-summed; sumErr != nil {
		return fail(sumErr)
	}
	if err != nil {
		return fail(err)
	}
	// Read back from the volume: docker answering "copied" is its word,
	// and a disk that filled halfway, or an extraction that skipped what
	// it could not write, is found here and not by the database.
	rr, rw := io.Pipe()
	go func() { rw.CloseWithError(d.ReadVolume(ctx, volume, rw)) }()
	onDisk, err := Summarize(rr, nil)
	_ = rr.CloseWithError(errors.New("stopped reading the volume back"))
	if err != nil {
		return fail(fmt.Errorf("reading %s back: %w", volume, err))
	}
	if onDisk != sent {
		return fail(fmt.Errorf("%s was not written as it was sent (%d files and %d bytes on disk, %d and %d sent, or the same with something in them different)", volume, onDisk.Files, onDisk.Bytes, sent.Files, sent.Bytes))
	}
	return onDisk, nil
}

// DockerCarrier reads a site's volumes from this machine's docker.
type DockerCarrier struct{ Docker runtime.Docker }

// SiteVolumes are domain's volumes here: the labelled and claimed ones,
// and the ones m mounts by name. A machine without docker has none; one
// whose docker does not answer is an error when the site runs in
// containers, and none when it does not.
func (c DockerCarrier) SiteVolumes(ctx context.Context, domain string, m *manifest.Manifest) ([]string, error) {
	vols, err := c.Docker.SiteVolumes(ctx, domain, NamedVolumes(domain, m)...)
	if err != nil && !UsesDocker(m) {
		return nil, nil
	}
	return vols, err
}

// VolumeUsers implements Carrier.
func (c DockerCarrier) VolumeUsers(ctx context.Context, volume string) ([]string, error) {
	return c.Docker.VolumeUsers(ctx, volume)
}

// ReadVolume implements Carrier.
func (c DockerCarrier) ReadVolume(ctx context.Context, volume string, w io.Writer) error {
	return c.Docker.ReadVolume(ctx, volume, w)
}

// Uncarried are the named volumes of a staged site's manifest that no
// move command of its workers fills: what a source that sends no volumes
// would have this node make empty.
func Uncarried(domain string, m *manifest.Manifest) []string {
	byCommand := map[string]bool{}
	for _, v := range MoveVolumes(domain, m, MovingWorkers(m)) {
		byCommand[v] = true
	}
	var out []string
	for _, v := range NamedVolumes(domain, m) {
		if !byCommand[v] {
			out = append(out, v)
		}
	}
	return out
}
