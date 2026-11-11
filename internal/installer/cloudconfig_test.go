package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// block is the content of the write_files entry for path, as cloud-init
// would write it: the lines under "content: |" with their indentation
// taken off.
func block(t *testing.T, doc, path string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "- path: "+path+"\n")
	if !ok {
		t.Fatalf("no write_files entry for %s", path)
	}
	_, rest, ok = strings.Cut(rest, "content: |\n")
	if !ok {
		t.Fatalf("%s has no content block", path)
	}
	var out []string
	for _, l := range strings.Split(rest, "\n") {
		if l == "" {
			out = append(out, "")
			continue
		}
		body, ok := strings.CutPrefix(l, "      ")
		if !ok {
			break
		}
		out = append(out, body)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// The file a new server boots with carries the code where the script
// reads it, and what it writes is run by a shell: the env file is sourced
// and the script parsed by sh here, since a quote out of place is only
// found on a server that already cost its fifteen minutes.
func TestCloudConfigCarriesTheCodeAndItsScriptsParse(t *testing.T) {
	code := "mwmesh_eyJuIjoiM2Q2Nz-_+/I4Nzc2ZTg1MzVhMSJ9=="
	doc, err := CloudConfig(code)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc, "#cloud-config\n") {
		t.Fatalf("cloud-init reads a file only when its first line is #cloud-config: %q", doc[:20])
	}
	if strings.Contains(doc, joinMark) || strings.Contains(doc, "\t") {
		t.Fatal("the mark is still there, or a tab that YAML refuses")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	script := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(env, []byte(block(t, doc, "/etc/middleware-bootstrap.env")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(block(t, doc, "/usr/local/sbin/middleware-bootstrap.sh")), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sh, "-c", `. "$1" && printf %s "$MIDDLEWARE_JOIN"`, "sh", env).CombinedOutput()
	if err != nil || string(out) != code {
		t.Fatalf("sourcing the env file gave %q, %v; want the code", out, err)
	}
	if out, err := exec.Command(sh, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("the bootstrap script does not parse: %v\n%s", err, out)
	}
	// The flags it passes are this binary's: a renamed one would fail on
	// the server, at the last step.
	body := block(t, doc, "/usr/local/sbin/middleware-bootstrap.sh")
	for _, want := range []string{"install --yes --browser no", `--join "$MIDDLEWARE_JOIN"`, `--endpoint "$MIDDLEWARE_ENDPOINT"`, "install --requirements --browser yes"} {
		if !strings.Contains(body, want) {
			t.Errorf("the script no longer runs %q", want)
		}
	}
}

// A code that could close the quotes it is written in is refused: the
// file is run by root on a new server.
func TestCloudConfigRefusesWhatIsNotACode(t *testing.T) {
	for _, code := range []string{"", `a"; rm -rf / #`, "a b", "a$(id)", "a`id`", "a\nb", `a\`} {
		if doc, err := CloudConfig(code); err == nil {
			t.Errorf("%q was written into the file:\n%s", code, doc[:40])
		}
	}
}
