package guard

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Text rules judge a line of text: a process's command line, or a line a
// site wrote to its log. Both are needed. Sampling the process table every
// three seconds misses a `cat` that lives for microseconds; the line the
// site logs about it stays. A command that is short and silent is seen by
// neither, and that is a real limit, not a bug more sampling would fix.

// TextRule is one signature.
type TextRule struct {
	Key         string `json:"key"`
	Level       string `json:"level"`
	Description string `json:"description"`
	match       func(s string) (string, bool)
}

var credentialMarks = []string{
	".ssh/id_rsa", ".ssh/id_ed25519", ".ssh/id_ecdsa", ".ssh/id_dsa", ".ssh/authorized_keys",
	"/etc/shadow", ".aws/credentials", ".git-credentials", ".docker/config.json", ".kube/config",
	".claude/.credentials.json", ".config/gh/hosts.yml", ".cloudflare/",
}

var (
	environRe  = regexp.MustCompile(`/proc/(self|\d+|\*)/environ`)
	downloadRe = []*regexp.Regexp{
		regexp.MustCompile(`\b(curl|wget|fetch)\b[^|;&]*\|\s*(sudo\s+)?(ba|z|da|k)?sh\b`),
		regexp.MustCompile(`\b(curl|wget|fetch)\b[^|;&]*\|\s*(sudo\s+)?(python3?|perl|ruby|node|php)\b`),
		regexp.MustCompile(`\b(ba|z)?sh\s+-c\s+["']?\$\(\s*(curl|wget)\b`),
		regexp.MustCompile(`\bbase64\s+(-d|--decode)\b[^|;&]*\|\s*(ba|z|da)?sh\b`),
	}
	sweepRe = []*regexp.Regexp{
		// A recursive grep over other people's folders, or for keys.
		regexp.MustCompile(`\bgrep\s+(-\S*\s+)*-\S*[rR]\S*\s.*(\s|^)(/home|/root|/var/www|/srv|/opt|/)(\s|/|$)`),
		regexp.MustCompile(`\bgrep\b.*(BEGIN [A-Z ]*PRIVATE KEY|AKIA\[|xkeysib-|sk_live_|SENDGRID_API_KEY)`),
		regexp.MustCompile(`\bfind\s+(/|/home|/root)(\s|/).*-i?name\s+\S*(id_rsa|id_ed25519|\.pem|\.env|credentials)`),
		regexp.MustCompile(`\bls\s+(-\S+\s+)*-\S*R\S*\s+(-\S+\s+)*(/home|/root|/)(\s|/|$)`),
	}
)

var textRules = []TextRule{
	{
		Key: "credentials", Level: LevelIntrusion,
		Description: "a site process reads credentials it has no business with: an SSH key, the machine's secrets, another process's environment",
		match: func(s string) (string, bool) {
			for _, m := range credentialMarks {
				if strings.Contains(s, m) {
					return "touches " + strconv.Quote(m), true
				}
			}
			if m := environRe.FindString(s); m != "" {
				return "reads " + m, true
			}
			return "", false
		},
	},
	{
		Key: "download-and-run", Level: LevelIntrusion,
		Description: "a script fetched from the network and handed straight to an interpreter",
		match: func(s string) (string, bool) {
			for _, re := range downloadRe {
				if m := re.FindString(s); m != "" {
					return strconv.Quote(m), true
				}
			}
			return "", false
		},
	},
	{
		Key: "credential-sweep", Level: LevelSuspicious,
		Description: "a sweep for keys or through other people's folders: nearly always explained, so it is reported and nothing is touched",
		match: func(s string) (string, bool) {
			for _, re := range sweepRe {
				if m := re.FindString(s); m != "" {
					return strconv.Quote(strings.TrimSpace(m)), true
				}
			}
			return "", false
		},
	},
}

// withoutOwnHome removes every token inside the site's own HOME (and its
// own folder), so what the site keeps there is not read as a theft. V1
// shut a site down for the preamble its own Claude Code writes before
// every Bash call, `source …/data/homes/<site>/.claude/shell-snapshots/…`:
// the wall that keeps a site from the machine's HOME, seen from inside.
// A token with ".." is kept: leaving the folder while starting in it.
// The whole token goes, not the prefix, because marks are substrings.
func withoutOwnHome(s string, own ...string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '=' })
	for _, f := range fields {
		for _, dir := range own {
			if dir == "" {
				continue
			}
			dir = filepath.Clean(dir) + "/"
			if strings.HasPrefix(f, dir) && !strings.Contains(f, "..") {
				s = strings.Replace(s, f, "", 1)
			}
		}
	}
	return s
}

// MatchText judges one line for a site whose HOME and folder are own.
func MatchText(line string, own ...string) (TextRule, string, bool) {
	s := withoutOwnHome(line, own...)
	for _, r := range textRules {
		if where, ok := r.match(s); ok {
			return r, where, true
		}
	}
	return TextRule{}, "", false
}

var (
	// A request line: an access log ("GET /x HTTP/1.1" 404) or a JSON
	// logger ("url":"/x" … "statusCode":404).
	requestRe = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) ([^" ]+)[^"]*" (\d{3})`)
	jsonURLRe = regexp.MustCompile(`"(url|originalUrl|path|uri)"\s*:\s*"([^"]*)"`)
	statusRe  = regexp.MustCompile(`"(statusCode|status)"\s*:\s*(\d{3})`)
)

// MatchLog judges a line a site wrote. What a client asked for is not
// what the site ran: on 26 August 2026 V1 shut a site down for a scanner
// asking for /.aws/credentials and getting a 404, and since the client
// writes the path, anyone on the internet could switch off any site by
// asking for a URL. So when the mark is in the requested path, a 4xx or
// 5xx is nothing and a 2xx or 3xx is a suspicion (the site gave something
// back where it should not), never an intrusion.
func MatchLog(line string, own ...string) (TextRule, string, bool) {
	r, where, ok := MatchText(line, own...)
	if !ok {
		return r, where, false
	}
	path, status := "", 0
	if m := requestRe.FindStringSubmatch(line); m != nil {
		path = m[2]
		status, _ = strconv.Atoi(m[3])
	} else if m := jsonURLRe.FindStringSubmatch(line); m != nil {
		path = m[2]
		if s := statusRe.FindStringSubmatch(line); s != nil {
			status, _ = strconv.Atoi(s[2])
		}
	}
	if path == "" {
		return r, where, true
	}
	if _, _, inPath := MatchText(path, own...); !inPath {
		// The mark is elsewhere in the line: the site's own doing.
		rest := strings.Replace(line, path, "", 1)
		if _, _, still := MatchText(rest, own...); still {
			return r, where, true
		}
		return TextRule{}, "", false
	}
	if status >= 400 || status == 0 {
		return TextRule{}, "", false
	}
	r.Level = LevelSuspicious
	return r, "answered " + strconv.Itoa(status) + " to " + strconv.Quote(path), true
}
