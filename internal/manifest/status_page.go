package manifest

import (
	"fmt"
	"strings"
)

// StatusPage is the site's public status page: its state now, its uptime
// over ninety days and its outages, as the house saw them. The house
// answers Path itself, before the site, so the page still answers when
// the site does not, which is when people open it.
type StatusPage struct {
	// Path is where it answers; empty is DefaultStatusPath.
	Path string `json:"path,omitempty"`
	// Title is the page's heading; empty is the site's name.
	Title string `json:"title,omitempty"`
}

// DefaultStatusPath is where the page answers unless told otherwise.
const DefaultStatusPath = "/status"

// Where is the path the page answers at.
func (s *StatusPage) Where() string {
	if s == nil || s.Path == "" {
		return DefaultStatusPath
	}
	return s.Path
}

func (s *StatusPage) valid() error {
	if s == nil {
		return nil
	}
	if s.Path != "" {
		// A plain path: the page is matched on it exactly, and "/" would
		// take the site's front page away.
		if !strings.HasPrefix(s.Path, "/") || s.Path == "/" || strings.ContainsAny(s.Path, "?#%* \t") || strings.Contains(s.Path, "//") || strings.Contains(s.Path, "..") {
			return fmt.Errorf("status_page.path: %q must be a plain path such as /status", s.Path)
		}
	}
	if len(s.Title) > 120 {
		return fmt.Errorf("status_page.title: at most 120 characters")
	}
	return nil
}
