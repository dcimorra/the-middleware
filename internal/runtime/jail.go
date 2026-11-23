package runtime

import (
	"os/exec"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Enclose, when set, puts a site's command in the site's jail
// (internal/jail) before it starts: its process, its workers, its tasks,
// a command run for it and a deploy's build. Set by the wiring when the
// machine can build jails; nil in tests and where it cannot, and then the
// command runs as it always did.
//
// The house's own commands (a task of no site, whose folder is the root)
// are the operator's and are not enclosed: spec.Domain is empty for them.
var Enclose func(cmd *exec.Cmd, spec *manifest.Spec, home string) error

func enclose(cmd *exec.Cmd, spec *manifest.Spec, home string) error {
	if Enclose == nil || spec.Domain == "" {
		return nil
	}
	return Enclose(cmd, spec, home)
}

// Guests, when set, answers which other sites' folders a site's container
// sees, those to read and those to write: the sites that take it in
// (internal/guests). Set by the wiring; asked when a container is
// started, so a container keeps what it started with, as a process keeps
// its jail's mounts.
var Guests func(site string) (read, write []string)
