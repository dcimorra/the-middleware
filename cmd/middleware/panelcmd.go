package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/settings"
)

func init() {
	subcommands["panel"] = panelCommand
	extraUsage += `  panel open [duration]   lift the panel's address lists and closed password
                          for a while (default 30m), when they shut you out
  panel close             end that early
  panel status            whether it is open, and until when
  panel key on|off|status hide the panel behind a key in its address
                          (?access-token=KEY); on makes a new one and
                          prints it, off is the way back if it is lost
`
}

// panelCommand is the fire exit. It needs no running supervisor, which is
// the point: it writes data/auth/panel-open, and the running one reads it
// on its next request. Opening is only from the shell; closing is also
// from the panel.
func panelCommand(args []string) error {
	root, rest, err := cliRoot("panel", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: middleware panel open [duration] | close | status | key on|off|status")
	}
	if rest[0] == "key" {
		return panelKey(root, rest[1:])
	}
	st := auth.New(root, events.New(filepath.Join(root, "data", "events")))
	switch rest[0] {
	case "open":
		d := 30 * time.Minute
		if len(rest) > 1 {
			if d, err = time.ParseDuration(rest[1]); err != nil {
				return fmt.Errorf("duration %q: %w", rest[1], err)
			}
		}
		until, err := st.OpenPanel(d)
		if err != nil {
			return err
		}
		fmt.Printf("The panel's address lists and closed password are lifted until %s.\nSign-in is still required. `middleware panel close` ends it.\n", until.Local().Format("15:04 on 2 Jan"))
		return nil
	case "close":
		if err := st.ClosePanel(); err != nil {
			return err
		}
		fmt.Println("Closed: the panel's rules apply again.")
		return nil
	case "status":
		if u := st.OpenUntil(); !u.IsZero() {
			fmt.Printf("open until %s\n", u.Local().Format(time.RFC3339))
		} else {
			fmt.Println("closed: the panel's rules apply")
		}
		return nil
	}
	return fmt.Errorf("unknown panel command %q", rest[0])
}

// panelKey works on data/inbound.json alone, like `inbound open`: the
// running process reads the file on its next request, so a lost key is
// undone from the shell with nothing else running.
func panelKey(root string, rest []string) error {
	if len(rest) != 1 {
		return errors.New("usage: middleware panel key on|off|status")
	}
	switch rest[0] {
	case "status":
		st, err := inbound.LoadState(root)
		if err != nil {
			return err
		}
		if st.PanelKey.On() {
			fmt.Printf("on since %s: the panel answers only with ?%s=<key>\n", st.PanelKey.Since.Local().Format(time.RFC3339), inbound.PanelKeyParam)
		} else {
			fmt.Println("off: the panel answers at its address")
		}
		return nil
	case "on":
		key, err := inbound.SetPanelKey(root, true)
		if err != nil {
			return err
		}
		addr := "https://<panel>"
		if s, err := settings.Load(root); err == nil && s.PanelDomain != "" {
			addr = "https://" + s.PanelDomain
		}
		fmt.Printf("The panel now answers only with its key. It is shown this once:\n\n  %s/?%s=%s\n\nAny earlier key no longer works. `middleware panel key off` ends it.\n", addr, inbound.PanelKeyParam, key)
		return nil
	case "off":
		if _, err := inbound.SetPanelKey(root, false); err != nil {
			return err
		}
		fmt.Println("Off: the panel answers at its address again.")
		return nil
	}
	return fmt.Errorf("unknown panel key command %q", rest[0])
}
