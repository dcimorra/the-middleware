package manifest

import (
	"fmt"
	"sort"
)

// What a guest may do in the site that takes it in.
const (
	// GuestRead is the folder read-only and what the house tells about
	// the site: how it is, its logs, its events, the copies of its
	// database.
	GuestRead = "read"
	// GuestWrite is the folder writable and every command the site's own
	// conversation has on it (restart, deploy, exec, tasks, backups).
	GuestWrite = "write"
)

// validGuests checks the sites this one takes in. A name that is no site
// here yet is not refused: the manifest is read on a node the guest may
// not be on (a copy, a site that moved), and nothing is lent to a name
// until a site of this node carries it.
func validGuests(gs map[string]string) error {
	for _, name := range GuestNames(gs) {
		if !ValidName(name) {
			return fmt.Errorf("guests: %q is not a site's name", name)
		}
		if lv := gs[name]; lv != GuestRead && lv != GuestWrite {
			return fmt.Errorf("guests.%s: want %q or %q, got %q", name, GuestRead, GuestWrite, lv)
		}
	}
	return nil
}

// GuestNames are gs's sites, sorted.
func GuestNames(gs map[string]string) []string {
	out := make([]string, 0, len(gs))
	for name := range gs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
