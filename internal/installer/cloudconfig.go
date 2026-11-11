package installer

import (
	_ "embed"
	"errors"
	"strings"
)

// cloudConfig is a cloud-init file ("user data") that turns a newly
// rented server into a node of the network on its first boot, with
// nobody at a terminal. It is kept here, next to the installer it calls,
// so the flags it passes are the ones this binary takes. It downloads
// the release itself instead of piping scripts/install.sh: the join has
// to happen before the slow steps (the browser, the system's upgrades),
// because an invitation lasts fifteen minutes.
//
//go:embed cloud-config.yaml
var cloudConfig string

// joinMark is where the invitation code goes in cloudConfig.
const joinMark = "@@MIDDLEWARE_JOIN@@"

// ErrBadJoinCode is a code CloudConfig will not write into the file.
var ErrBadJoinCode = errors.New("not an invitation code")

// CloudConfig is the cloud-init file with the invitation code in it. The
// code ends up inside double quotes in a file root's shell reads, so
// anything but the characters an invitation is made of (base64 and its
// prefix) is refused rather than escaped: an escape that is wrong once
// is a command run as root on a new server.
func CloudConfig(code string) (string, error) {
	if code == "" {
		return "", ErrBadJoinCode
	}
	for _, c := range code {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '+' || c == '/' || c == '=':
		default:
			return "", ErrBadJoinCode
		}
	}
	return strings.Replace(cloudConfig, joinMark, code, 1), nil
}
