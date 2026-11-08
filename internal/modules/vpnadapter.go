package modules

import (
	"context"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/modules/vpn"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// The vpn package was written against its own copy of the Module shape so
// it could compile on its own; this file is the one place where that
// shape and this package's meet. The differences are small (its Install
// returns lines, its Enable verifies again and returns lines) and they are
// absorbed here rather than in the vpn package, whose tests pin its own
// signatures.

// VPNAdapter makes a *vpn.VPN a Module.
type VPNAdapter struct {
	*vpn.VPN
}

// VPNModule wraps v.
func VPNModule(v *vpn.VPN) *VPNAdapter { return &VPNAdapter{VPN: v} }

// NodeBound implements NodeBound: the VPN's keys, port and subnet are this
// server's; another node copying them would answer as this one.
func (a *VPNAdapter) NodeBound() bool { return true }

// Needs converts the vpn package's needs. A need with a default is
// optional here: the vpn package applies the default itself when the
// value is empty.
func (a *VPNAdapter) Needs() []Need {
	src := a.VPN.Needs()
	out := make([]Need, 0, len(src))
	for _, n := range src {
		help := ""
		switch n.Default {
		case "":
		case "detected":
			help = "Empty: this machine's public address, found by itself, with the port below."
		default:
			help = "Default " + n.Default + "."
		}
		out = append(out, Need{Key: n.Key, Label: n.Label, Secret: n.Secret, Optional: n.Default != "", Help: help})
	}
	return out
}

// Verify implements Module.
func (a *VPNAdapter) Verify(ctx context.Context, cfg Config) []Line {
	return convertLines(a.VPN.Verify(ctx, cfg))
}

// Install implements Module. The vpn package reports the install as lines
// because it cannot install anything itself (the helper runs no package
// manager); a failed line is the error here.
func (a *VPNAdapter) Install(ctx context.Context, cfg Config, progress func(string)) error {
	lines := convertLines(a.VPN.Install(ctx, cfg))
	for _, l := range lines {
		progress(l.Text)
	}
	if Failed(lines) {
		return &lineError{lines: lines}
	}
	return nil
}

// Enable implements Module.
func (a *VPNAdapter) Enable(ctx context.Context, cfg Config) error {
	lines, err := a.VPN.Enable(ctx, cfg)
	if err != nil {
		return &lineError{lines: convertLines(lines), err: err}
	}
	return nil
}

// Disable implements Module.
func (a *VPNAdapter) Disable(ctx context.Context) error { return a.VPN.Disable(ctx) }

// Routes implements Module. The vpn package's keys are ServeMux patterns
// ("GET /{$}", "DELETE /peers/{name}"); the API's module dispatcher knows
// to build a mux from keys of that shape.
func (a *VPNAdapter) Routes() map[string]http.Handler { return a.VPN.Routes() }

// lineError carries the lines of a failed step so the message names the
// probe that failed, not just "enable: verification failed".
type lineError struct {
	lines []Line
	err   error
}

func (e *lineError) Error() string {
	if msg := firstFailure(e.lines); msg != "" {
		return msg
	}
	if e.err != nil {
		return e.err.Error()
	}
	return "failed"
}

func (e *lineError) Unwrap() error { return e.err }

func convertLines(src []vpn.Line) []Line {
	out := make([]Line, 0, len(src))
	for _, l := range src {
		out = append(out, Line{Status: l.Status, Text: l.Text})
	}
	return out
}

// BoxSecrets adapts a *secrets.Box, which works on strings, to the vpn
// package's Secrets, which works on key bytes. A Go string carries any
// bytes, so the conversion loses nothing; the point of the adapter is that
// the VPN's keys end up in the same format and under the same key as every
// other credential of the installation, so the CLI and the service can
// read what the other wrote.
type BoxSecrets struct {
	Box *secrets.Box
}

// OpenBoxSecrets opens the installation's box.
func OpenBoxSecrets(root string) (*BoxSecrets, error) {
	box, err := secrets.Open(root)
	if err != nil {
		return nil, err
	}
	return &BoxSecrets{Box: box}, nil
}

// Encrypt implements vpn.Secrets.
func (b *BoxSecrets) Encrypt(plain []byte) (string, error) { return b.Box.Encrypt(string(plain)) }

// Decrypt implements vpn.Secrets.
func (b *BoxSecrets) Decrypt(stored string) ([]byte, error) {
	s, err := b.Box.Decrypt(stored)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

var (
	_ Module      = (*VPNAdapter)(nil)
	_ vpn.Secrets = (*BoxSecrets)(nil)
)
