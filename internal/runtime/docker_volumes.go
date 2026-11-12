package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A site's named volumes are its data as much as its folder is, and a
// move that carried only the folder left the database behind: the site
// arrived, its Postgres found an empty volume, ran initdb, and the web
// seeded a demo catalogue over what had been a shop with orders. What is
// here reads a volume out as a tar and writes one in, so a move can take
// them (relocate.Mover).

// VolumeMount is where a volume is mounted in the container it is read
// and written through, and so the first element of every name in its tar.
const VolumeMount = "volume"

// emptyImage is an image with nothing in it, made here from an empty
// tar. A volume is read and written through a container that is created
// and never started (docker cp), so the image needs no tar, no shell and
// no network to pull it: alpine was the first idea, and a node behind
// Docker Hub's rate limit could not have moved a site.
const emptyImage = "middleware-volume:empty"

// SiteVolumes are the volumes on this machine that are site's: the ones
// labelled with it, the ones from before labels that it claimed
// (VolumeClaimsFile), and those of named (the volumes its manifest
// mounts, by their docker names) that are here and nobody else's. An
// error is a docker that did not answer: "none" and "could not ask" must
// not look the same to a move.
func (d Docker) SiteVolumes(ctx context.Context, site string, named ...string) ([]string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), "volume", "ls", "-q", "--filter", "label="+SiteLabel+"="+site).Output()
	if err != nil {
		return nil, fmt.Errorf("docker volume ls: %w", err)
	}
	seen := map[string]bool{}
	for _, v := range strings.Fields(string(out)) {
		// The filter matches a label that begins so on some versions:
		// asked again, by name.
		if owner, ok := d.volumeOwner(ctx, v); ok && owner == site {
			seen[v] = true
		}
	}
	claims := map[string]string{}
	if d.Data != "" {
		if raw, err := os.ReadFile(filepath.Join(d.Data, VolumeClaimsFile)); err == nil {
			_ = json.Unmarshal(raw, &claims)
		}
		for v, owner := range claims {
			if owner == site && d.VolumeExists(ctx, v) {
				seen[v] = true
			}
		}
	}
	for _, v := range named {
		owner, ok := d.volumeOwner(ctx, v)
		if ok && (owner == site || owner == "" && (claims[v] == "" || claims[v] == site)) {
			seen[v] = true
		}
	}
	vols := make([]string, 0, len(seen))
	for v := range seen {
		vols = append(vols, v)
	}
	sort.Strings(vols)
	return vols, nil
}

// VolumeUsers are the running containers that mount the volume: read
// while one writes, a database's files are a torn copy.
func (d Docker) VolumeUsers(ctx context.Context, volume string) ([]string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "--filter", "volume="+volume, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// ensureEmptyImage makes emptyImage when this docker does not have it.
func (d Docker) ensureEmptyImage(ctx context.Context) error {
	if exec.CommandContext(ctx, d.bin(), "image", "inspect", emptyImage).Run() == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, d.bin(), "import", "-", emptyImage)
	// An empty tar: two blocks of zeros.
	cmd.Stdin = bytes.NewReader(make([]byte, 1024))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker import (the empty image volumes are copied through): %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// holder creates, and never starts, a container that mounts the volume,
// and returns its name and how to remove it.
func (d Docker) holder(ctx context.Context, volume string, readOnly bool) (string, func(), error) {
	if err := d.ensureEmptyImage(ctx); err != nil {
		return "", nil, err
	}
	mount := volume + ":/" + VolumeMount
	if readOnly {
		mount += ":ro"
	}
	// --network none and no restart: it never runs, and must not be
	// something that could.
	out, err := exec.CommandContext(ctx, d.bin(), "create", "--network", "none", "-v", mount, emptyImage, "/none").CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("docker create (to reach the volume %s): %w: %s", volume, err, bytes.TrimSpace(out))
	}
	id := lastLine(string(out))
	return id, func() {
		// On its own time: the caller's may be what ran out.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(rctx, d.bin(), "rm", "-f", id).Run()
	}, nil
}

// ReadVolume writes the volume to w as a tar, every name under
// VolumeMount, the volume's own folder first with its owner and mode.
// The volume is mounted read only: nothing here can change it.
func (d Docker) ReadVolume(ctx context.Context, volume string, w io.Writer) error {
	if !d.VolumeExists(ctx, volume) {
		return fmt.Errorf("the docker volume %s is not on this machine", volume)
	}
	id, remove, err := d.holder(ctx, volume, true)
	if err != nil {
		return err
	}
	defer remove()
	cmd := exec.CommandContext(ctx, d.bin(), "cp", id+":/"+VolumeMount, "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// A pipe read here, not w handed to the command: Wait would otherwise
	// sit behind a copy it cannot interrupt when w stops taking.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, copyErr := io.Copy(w, stdout)
	if copyErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if copyErr != nil {
		return fmt.Errorf("reading the volume %s: %w", volume, copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("docker cp (reading the volume %s): %w: %s", volume, waitErr, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// WriteVolume lays a tar as ReadVolume writes it over the volume, which
// must be there: owners, modes and links as the tar says them, the
// volume's own folder included.
func (d Docker) WriteVolume(ctx context.Context, volume string, r io.Reader) error {
	if !d.VolumeExists(ctx, volume) {
		return fmt.Errorf("the docker volume %s is not on this machine", volume)
	}
	id, remove, err := d.holder(ctx, volume, false)
	if err != nil {
		return err
	}
	defer remove()
	// Into the container's root, where the tar's first name is the mount:
	// extracted into the mount itself, the volume's own folder kept the
	// owner docker gave it, and a database that runs as another user
	// without the capability to chown could not write in its own volume.
	cmd := exec.CommandContext(ctx, d.bin(), "cp", "-", id+":/")
	cmd.Stdin = r
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp (writing the volume %s): %w: %s", volume, err, bytes.TrimSpace(out))
	}
	return nil
}
