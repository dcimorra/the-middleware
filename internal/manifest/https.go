package manifest

import (
	"fmt"
	"strings"
)

// ForcesHTTPS reports whether host is one of the names the site keeps to
// https (force_https): a request for it over plain http is sent to the
// same address on https before anything else is looked at.
func (m *Manifest) ForcesHTTPS(host string) bool {
	if m == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range m.ForceHTTPS {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(h), "."), host) {
			return true
		}
	}
	return false
}

// validForceHTTPS checks the names, not that the site answers them: an
// alias taken away leaves its line behind, harmless, and refusing the
// whole manifest for it would block the next unrelated change.
func validForceHTTPS(names []string) error {
	for _, h := range names {
		if !ValidDomain(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))) {
			return fmt.Errorf("force_https: %q is not a domain", h)
		}
	}
	return nil
}
