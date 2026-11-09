package crew

import "slices"

// Capability is something the house does for an agent, and the vault
// names it needs to. Every agent has them all (the operator wants them
// to be able to do whatever their goal takes); one that lacks a name is
// "not ready", the agent is told which, and asks for them when it needs
// them.
type Capability struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	What  string `json:"what"`
	// Needs are the vault names it reads; Optional ones have a default.
	Needs    []string `json:"needs,omitempty"`
	Optional []string `json:"optional,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	Ready    bool     `json:"ready"`
}

// catalog is every capability, in the order the prompt lists them.
var catalog = []Capability{
	{Key: "shell", Title: "Shell and the web", What: "your desk, a shell with the machine's tools, and the internet: curl, any public API, anything you can install in your desk."},
	{Key: "mail-out", Title: "Sending email", What: "`middleware crew mail send` sends from your own address over SMTP (465 is TLS from the start, any other port STARTTLS).", Needs: []string{"SMTP_HOST", "SMTP_USER", "SMTP_PASSWORD"}, Optional: []string{"SMTP_PORT", "SMTP_FROM"}},
	{Key: "mail-in", Title: "Reading email", What: "`middleware crew mail list` and `read` read your inbox over IMAP (993, TLS): verification codes and links arrive there. IMAP_USER and IMAP_PASSWORD default to the SMTP ones, IMAP_HOST to SMTP_HOST.", Needs: []string{"IMAP_HOST|SMTP_HOST", "IMAP_USER|SMTP_USER", "IMAP_PASSWORD|SMTP_PASSWORD"}, Optional: []string{"IMAP_PORT"}},
}

// capabilities is the catalog against the names a vault holds. A need
// written "A|B" is met by either.
func capabilities(keys []string) []Capability {
	out := make([]Capability, 0, len(catalog))
	for _, cp := range catalog {
		cp.Missing = nil
		for _, n := range cp.Needs {
			ok := false
			for _, alt := range splitAlt(n) {
				if slices.Contains(keys, alt) {
					ok = true
				}
			}
			if !ok {
				cp.Missing = append(cp.Missing, splitAlt(n)[0])
			}
		}
		cp.Ready = len(cp.Missing) == 0
		out = append(out, cp)
	}
	return out
}

func splitAlt(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '|' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
