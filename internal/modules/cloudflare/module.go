package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/modules"
)

// Key is the module's key.
const Key = "cloudflare"

// BinaryName is the connector's name under modules/bin.
const BinaryName = "cloudflared"

// ReleaseURL is where the connector is downloaded from, with the
// architecture substituted. Tests point it at a fake.
var ReleaseURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-%s"

// ReleaseSHA256 may be set to pin the download when the operator knows
// the checksum of the release they want. GitHub's "latest" URL changes
// under a stable name, so no checksum ships with the binary by default
// and the install says so in its progress lines.
var ReleaseSHA256 = ""

// Module is the Cloudflare module: DNS records and tunnel routes for the
// domains this installation serves, plus the connector binary and its
// config so the installer can run the tunnel from this machine.
type Module struct {
	mgr *modules.Manager
	// Service is what tunnel routes point at: the loopback listener.
	// The app sets it from settings.Listen before enabling.
	Service string
	// BaseURL overrides the API for tests.
	BaseURL string
	HTTP    *http.Client
	// Connector runs cloudflared; nil in the CLI, which only edits config.
	Connector *Connector
}

// New builds the module over the manager that holds its config.
func New(mgr *modules.Manager) *Module { return &Module{mgr: mgr} }

func (m *Module) Key() string  { return Key }
func (m *Module) Name() string { return "Cloudflare" }
func (m *Module) Description() string {
	return "DNS records and tunnel routes for the domains served here, managed from the panel"
}

func (m *Module) Needs() []modules.Need {
	return []modules.Need{
		{Key: "token", Label: "API token", Secret: true, Help: "An API token with Zone:DNS:Edit on the zones to manage, and Account:Cloudflare Tunnel:Edit if a tunnel is used. Created at dash.cloudflare.com/profile/api-tokens."},
		{Key: "account_id", Label: "Account id", Optional: true, Help: "Found when empty, if the token sees exactly one account."},
		{Key: "tunnel_id", Label: "Tunnel id", Optional: true, Help: "The tunnel this server runs, to publish through it. Empty is DNS only: sites are reached on 80 and 443. Never taken by itself: a tunnel of another server would split its traffic with this one.", Local: true},
	}
}

// Binaries implements modules.Binaries.
func (m *Module) Binaries() []string { return []string{BinaryName} }

// BinariesFor implements modules.BinariesFor: cloudflared only with a
// tunnel, as Install does; DNS alone needs no binary.
func (m *Module) BinariesFor(cfg modules.Config) []string {
	if cfg["tunnel_id"] == "" {
		return nil
	}
	return m.Binaries()
}

// client builds a Client for a config, without touching the store.
func (m *Module) client(cfg modules.Config) *Client {
	return &Client{Token: cfg["token"], AccountID: cfg["account_id"], TunnelID: cfg["tunnel_id"], Service: m.Service, BaseURL: m.BaseURL, HTTP: m.HTTP}
}

// Client returns a client over the stored, decrypted config: what the
// inbound door uses to publish a domain.
func (m *Module) Client() (*Client, error) {
	cfg, err := m.mgr.Config(Key)
	if err != nil {
		return nil, err
	}
	if cfg["token"] == "" {
		return nil, fmt.Errorf("cloudflare: not configured")
	}
	return m.client(cfg), nil
}

// Verify probes every read the module relies on and names what it will
// not probe. Each line is one permission, so an operator who made the
// token with one scope missing sees which one. The account and tunnel
// ids it discovers are written into cfg: the manager hands the same map
// to Install and saves it, so what verify found is what gets installed.
func (m *Module) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	c := m.client(cfg)
	var lines []modules.Line
	status, err := c.TokenStatus(ctx)
	switch {
	case err != nil && c.AccountID == "":
		return append(lines, modules.Fail("token: "+err.Error()+" (an account token, cfat_…, is verified against its account: set account_id)"))
	case err != nil:
		return append(lines, modules.Fail("token: "+err.Error()))
	case status != "active":
		return append(lines, modules.Fail("token is "+status))
	}
	lines = append(lines, modules.OK("token is valid and active"))

	zones, err := c.Zones(ctx)
	if err != nil {
		lines = append(lines, modules.Fail("cannot list zones (Zone:Zone:Read is missing): "+err.Error()))
	} else if len(zones) == 0 {
		lines = append(lines, modules.Fail("the token sees no zones: it needs Zone:Zone:Read on the zones to manage"))
	} else {
		names := make([]string, 0, 3)
		for i, z := range zones {
			if i == 3 {
				names = append(names, "…")
				break
			}
			names = append(names, z.Name)
		}
		lines = append(lines, modules.OK(fmt.Sprintf("can list zones: %d (%s)", len(zones), strings.Join(names, ", "))))
		if recs, err := c.DNSRecords(ctx, zones[0].ID); err != nil {
			lines = append(lines, modules.Fail(fmt.Sprintf("cannot read DNS records of %s (Zone:DNS:Read is missing): %v", zones[0].Name, err)))
		} else {
			lines = append(lines, modules.OK(fmt.Sprintf("can read DNS records of %s: %d records", zones[0].Name, len(recs))))
		}
	}

	if c.AccountID == "" {
		accts, err := c.Accounts(ctx)
		switch {
		case err != nil:
			lines = append(lines, modules.Fail("cannot list accounts to find the account id (Account:Account Settings:Read is missing, or set account_id): "+err.Error()))
		case len(accts) == 1:
			c.AccountID = accts[0].ID
			cfg["account_id"] = accts[0].ID
			lines = append(lines, modules.OK(fmt.Sprintf("found account %s (%s)", accts[0].Name, accts[0].ID)))
		case len(accts) == 0 && len(zones) > 0 && zones[0].Account.ID != "":
			c.AccountID = zones[0].Account.ID
			cfg["account_id"] = c.AccountID
			lines = append(lines, modules.OK("found account "+c.AccountID+" from the first zone"))
		default:
			names := make([]string, 0, len(accts))
			for _, a := range accts {
				names = append(names, a.Name+" ("+a.ID+")")
			}
			lines = append(lines, modules.Fail("the token sees "+fmt.Sprint(len(accts))+" accounts, set account_id to one of: "+strings.Join(names, ", ")))
		}
	}

	// A tunnel is used only when chosen. Taking the account's only one by
	// itself started its connector here, and a tunnel that is another
	// server's then splits that server's traffic with this one: nobody
	// asked for that, and nothing would say it was happening. Without a
	// tunnel id the module is DNS only, which is also what a house that
	// serves on 80 and 443 wants.
	if c.AccountID != "" && c.TunnelID == "" {
		tunnels, err := c.Tunnels(ctx)
		switch {
		case err != nil:
			lines = append(lines, modules.Skip("no tunnel chosen: DNS only (the tunnels could not be listed: "+err.Error()+")"))
		case len(tunnels) == 0:
			lines = append(lines, modules.Skip("no tunnel chosen: DNS only. The account has no tunnel; create one in Cloudflare and set tunnel_id to route through it"))
		default:
			names := make([]string, 0, len(tunnels))
			for _, t := range tunnels {
				names = append(names, t.Name+" ("+t.ID+")")
			}
			lines = append(lines, modules.Skip("no tunnel chosen: DNS only. To route through a tunnel, set tunnel_id to one that is this server's: "+strings.Join(names, ", ")))
		}
	}

	if c.AccountID != "" && c.TunnelID != "" {
		rules, err := c.TunnelIngress(ctx, "")
		if err != nil {
			lines = append(lines, modules.Fail("cannot read the tunnel's ingress rules (Account:Cloudflare Tunnel:Read is missing, or the tunnel id is wrong): "+err.Error()))
		} else {
			n := 0
			for _, r := range rules {
				if r.Hostname != "" {
					n++
				}
			}
			lines = append(lines, modules.OK(fmt.Sprintf("can reach tunnel %s and read its %d ingress rules", c.TunnelID, n)))
		}
	}

	lines = append(lines, modules.Skip("write permissions (Zone:DNS:Edit, Account:Cloudflare Tunnel:Edit) are not probed: probing them would create a real record or route. The first publish will say if one is missing."))
	return lines
}

// Install downloads the connector and writes its config. Running the
// connector is the installer's job: this module only produces
//
//	modules/bin/cloudflared                 the binary
//	modules/cloudflared/config.yml          tunnel id, metrics address, protocol
//	modules/cloudflared/token               the tunnel token, mode 0600
//
// and the Connector runs, for as long as the module is enabled,
//
//	modules/bin/cloudflared --config modules/cloudflared/config.yml tunnel --no-autoupdate run --token-file modules/cloudflared/token
//
// The ingress rules are not in config.yml on purpose: they live in the
// API (remotely managed tunnel), which is where Publish writes them, so
// there is one list and not two that drift apart.
func (m *Module) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	if cfg["tunnel_id"] == "" {
		progress("no tunnel id: DNS only, the connector is not installed")
		return nil
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64", "arm64", "386", "arm":
	default:
		return fmt.Errorf("no cloudflared release for %s", arch)
	}
	root := m.mgr.Root()
	binDir := filepath.Join(root, "modules", "bin")
	if _, err := os.Stat(filepath.Join(binDir, BinaryName)); err != nil {
		if _, err := modules.InstallBinary(ctx, binDir, BinaryName, fmt.Sprintf(ReleaseURL, arch), ReleaseSHA256, progress); err != nil {
			return err
		}
	} else {
		progress("cloudflared already installed, keeping it")
	}
	dir := filepath.Join(root, "modules", "cloudflared")
	config := fmt.Sprintf("# Written by the cloudflare module. Ingress rules live in the API, not here.\ntunnel: %s\nmetrics: %s\nprotocol: auto\nno-autoupdate: true\n", cfg["tunnel_id"], MetricsAddr)
	if err := fsx.WriteAtomic(filepath.Join(dir, "config.yml"), []byte(config), 0o644); err != nil {
		return err
	}
	progress("wrote " + filepath.Join(dir, "config.yml"))
	token, err := m.client(cfg).TunnelToken(ctx, cfg["tunnel_id"])
	if err != nil {
		// Routes can still be managed without it; what cannot happen is
		// running the connector from this machine, and the line says so.
		progress("could not fetch the tunnel token (" + err.Error() + "): routes can be managed, but running the connector here needs the token in " + filepath.Join(dir, "token"))
		return nil
	}
	if err := fsx.WriteAtomic(filepath.Join(dir, "token"), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	progress("wrote the tunnel token to " + filepath.Join(dir, "token"))
	return nil
}

// Enable has nothing left to do: Verify filled the ids and Install wrote
// the files. It stays a method so the interface reads the same for every
// module.
func (m *Module) Enable(ctx context.Context, cfg modules.Config) error { return nil }

// Disable has nothing to do itself: the Connector sees the module off on
// its next pass and stops cloudflared, and routes already written stay.
func (m *Module) Disable(ctx context.Context) error { return nil }

// Routes exposes read-only state for the panel.
func (m *Module) Routes() map[string]http.Handler {
	out := map[string]http.Handler{
		"GET /zones":     http.HandlerFunc(m.handleZones),
		"GET /ingress":   http.HandlerFunc(m.handleIngress),
		"GET /connector": http.HandlerFunc(m.handleConnector),
	}
	for k, h := range m.extraRoutes() {
		out[k] = h
	}
	return out
}

func (m *Module) handleConnector(w http.ResponseWriter, r *http.Request) {
	if m.Connector == nil {
		writeError(w, http.StatusNotImplemented, errNoConnector)
		return
	}
	writeJSON(w, m.Connector.Status())
}

func (m *Module) handleZones(w http.ResponseWriter, r *http.Request) {
	c, err := m.Client()
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	zones, err := c.Zones(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, zones)
}

func (m *Module) handleIngress(w http.ResponseWriter, r *http.Request) {
	c, err := m.Client()
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	rules, err := c.TunnelIngress(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{"tunnel_id": c.TunnelID, "target": c.TunnelTarget(), "ingress": rules})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
