package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The copy of a database made on the node a site left has to reach the
// site where it now is: sent from one node's API, through the mesh, it
// is in the site's HOME on the other, whole, and nowhere it is served.
func TestAFileIsSentToASiteOnAnotherNode(t *testing.T) {
	port := meshPort(t)
	a, _ := nodeFixture(t, "alpha", port)
	b, _ := nodeFixture(t, "beta", port)
	_, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res, out := b.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %s", text(out))
	}
	if res, out := b.call("POST", "/api/v1/sites", map[string]any{"domain": "s.test", "template": "static", "publish": false}); res.StatusCode != 201 {
		t.Fatalf("site: %s", text(out))
	}
	dump := bytes.Repeat([]byte("a volume's tar, gzipped\n"), 100000)
	file := filepath.Join(t.TempDir(), "mw-s-test-db.tar.gz")
	os.WriteFile(file, dump, 0o600)
	sum := sha256.Sum256(dump)

	// By the node's name, as the operator knows it.
	res, out := a.call("POST", "/api/v1/nodes/beta/send", map[string]string{"path": file, "site": "s.test"})
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
	if out["path"] != "~/inbox/mw-s-test-db.tar.gz" || out["sha256"] != hex.EncodeToString(sum[:]) || out["node"] != "beta" || int(out["bytes"].(float64)) != len(dump) {
		t.Fatalf("answer: %s", text(out))
	}
	inbox := filepath.Join(b.root, "data", "homes", "s.test", "inbox")
	got, err := os.ReadFile(filepath.Join(inbox, "mw-s-test-db.tar.gz"))
	if err != nil || !bytes.Equal(got, dump) {
		t.Fatalf("in the site's inbox: %d bytes, %v", len(got), err)
	}
	if info, _ := os.Stat(filepath.Join(inbox, "mw-s-test-db.tar.gz")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(b.root, "sites", "s.test", "inbox")); err == nil {
		t.Fatal("it landed in the folder the site serves from")
	}
	if left, _ := filepath.Glob(filepath.Join(inbox, ".*")); len(left) != 0 {
		t.Fatalf("parts left behind: %v", left)
	}
	// Under another name, replacing what was there.
	os.WriteFile(file, []byte("second"), 0o600)
	if res, out := a.call("POST", "/api/v1/nodes/beta/send", map[string]string{"path": file, "site": "s.test", "name": "db.tar.gz"}); res.StatusCode != 200 || out["path"] != "~/inbox/db.tar.gz" {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}

	for name, c := range map[string]struct {
		node string
		body map[string]string
		code int
		want string
	}{
		"a site that is not there":  {"beta", map[string]string{"path": file, "site": "nope.test"}, 502, "not a site on this node"},
		"a node that is not there":  {"gamma", map[string]string{"path": file, "site": "s.test"}, 404, "no node of the network"},
		"a folder":                  {"beta", map[string]string{"path": filepath.Dir(file), "site": "s.test", "name": "x"}, 400, "not a file"},
		"a name with a folder":      {"beta", map[string]string{"path": file, "site": "s.test", "name": "../site.json"}, 400, "not a file name"},
		"a hidden name":             {"beta", map[string]string{"path": file, "site": "s.test", "name": ".profile"}, 400, "not a file name"},
		"a path that is not whole":  {"beta", map[string]string{"path": "db.tar.gz", "site": "s.test"}, 400, "whole path"},
		"a file that is not there":  {"beta", map[string]string{"path": file + ".no", "site": "s.test"}, 400, "no such file"},
		"a name that is not a site": {"beta", map[string]string{"path": file, "site": "../x"}, 400, "not a site's name"},
	} {
		res, out := a.call("POST", "/api/v1/nodes/"+c.node+"/send", c.body)
		if res.StatusCode != c.code || !strings.Contains(text(out), c.want) {
			t.Fatalf("%s: %d %s", name, res.StatusCode, text(out))
		}
	}
	if _, err := os.Stat(filepath.Join(b.root, "sites", "site.json")); err == nil {
		t.Fatal("a name climbed out of the inbox")
	}
}

// A token that opens one site does not write into its inbox, nor read
// this machine's files to send them: both are the operator's.
func TestOnlyTheOperatorSendsAndFillsAnInbox(t *testing.T) {
	port := meshPort(t)
	a, _ := nodeFixture(t, "alpha", port)
	if res, out := a.call("POST", "/api/v1/sites", map[string]any{"domain": "s.test", "template": "static", "publish": false}); res.StatusCode != 201 {
		t.Fatalf("site: %s", text(out))
	}
	_, secret, err := a.srv.opts.Auth.CreateScopedToken("one site", []string{"site:s.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{
		{"PUT", "/api/v1/sites/s.test/inbox/x.txt"},
		{"POST", "/api/v1/nodes/beta/send"},
	} {
		req, _ := http.NewRequest(c.method, a.http.URL+c.path, strings.NewReader(`{"path":"/etc/hostname","site":"s.test"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with a site's token: %d", c.method, c.path, res.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(a.root, "data", "homes", "s.test", "inbox", "x.txt")); err == nil {
		t.Fatal("a site's token wrote into the inbox")
	}
}
