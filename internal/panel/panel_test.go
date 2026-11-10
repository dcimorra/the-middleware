package panel

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/relocate"
)

func get(t *testing.T, h http.Handler, p string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	return rec
}

func TestTheShellAnswersTheRootAndDeepLinks(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/sites/x", "/settings", "/anything/at/all"} {
		rec := get(t, h, p)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: content type %q", p, ct)
		}
		if !strings.Contains(rec.Body.String(), "<title>") {
			t.Fatalf("%s: body is not the shell", p)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control %q", p, cc)
		}
	}
}

func TestAssetsCarryTheirContentType(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"/assets/app.js":       "text/javascript",
		"/assets/app.css":      "text/css",
		"/assets/i18n/en.json": "application/json",
	}
	for p, want := range cases {
		rec := get(t, h, p)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Fatalf("%s: content type %q, want %s", p, ct, want)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("%s: no ETag", p)
		}
	}
}

func TestAMissingAssetIsA404NotTheShell(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/assets/nope.js", "/assets/", "/assets/index.html", "/assets/../index.html"} {
		rec := get(t, h, p)
		if rec.Code != 404 {
			t.Fatalf("%s: status %d, body %q", p, rec.Code, rec.Body.String()[:40])
		}
	}
}

func TestTheETagSavesTheSecondDownload(t *testing.T) {
	h := Handler()
	first := get(t, h, "/assets/app.js")
	tag := first.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d with matching ETag", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("a 304 carried a body")
	}
}

func TestEveryResponseCarriesTheCSP(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/assets/app.js", "/assets/missing.css"} {
		rec := get(t, h, p)
		if got := rec.Header().Get("Content-Security-Policy"); got != CSP {
			t.Fatalf("%s: CSP %q", p, got)
		}
	}
}

func TestOnlyGetAndHeadAreAnswered(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func walkJS(t *testing.T, fn func(name, src string)) {
	t.Helper()
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") {
			return nil
		}
		data, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		fn(p, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The CSP forbids inline script and style, so a stray style="" or onclick=
// in the shell would render as nothing, with an error only the console
// sees. Same for the JavaScript: a style attribute set through setAttribute
// or innerHTML is blocked by style-src and looks like a layout bug.
func TestNothingInlineSlipsPastTheCSP(t *testing.T) {
	data, err := fs.ReadFile(FS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, bad := range []string{"<style", " style=", "onclick=", "onload=", "javascript:", "<script>"} {
		if strings.Contains(s, bad) {
			t.Fatalf("index.html contains %q", bad)
		}
	}
	walkJS(t, func(name, src string) {
		// style: handed to h() becomes a style attribute too, which the
		// CSP drops: the specs progress bar never filled that way.
		for _, bad := range []string{`setAttribute("style"`, `setAttribute('style'`, ` style="`, `{ style:`, `, style:`, `.innerHTML`, `.outerHTML`, `insertAdjacentHTML`} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s, which the CSP blocks or which bypasses escaping", name, bad)
			}
		}
	})
}

// A literal key is one followed by ) or ,; t("mode." + mode) builds the
// key at runtime and is covered by the prefix check below.
var tCall = regexp.MustCompile(`\bt\(\s*"([^"\\]+)"\s*[,)]`)
var tPrefix = regexp.MustCompile(`\bt\(\s*"([^"\\]+\.)"\s*\+`)

// Every user-visible string goes through the catalog, so a key that is used
// in the JavaScript and missing from en.json would show as the key itself,
// in production, to the operator. Node is not available to check this, so
// the Go test greps the embedded sources for literal keys.
func TestEveryStringKeyUsedInTheJavaScriptExistsInTheCatalog(t *testing.T) {
	data, err := fs.ReadFile(FS, "i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]string
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("en.json is not a flat object of strings: %v", err)
	}
	used := map[string]bool{}
	walkJS(t, func(name, src string) {
		for _, m := range tCall.FindAllStringSubmatch(src, -1) {
			used[m[1]] = true
			if _, ok := catalog[m[1]]; !ok {
				t.Errorf("%s uses t(%q) which is not in en.json", name, m[1])
			}
		}
		for _, m := range tPrefix.FindAllStringSubmatch(src, -1) {
			found := false
			for k := range catalog {
				if strings.HasPrefix(k, m[1]) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s builds keys from prefix %q and en.json has none", name, m[1])
			}
		}
	})
	if len(used) < 50 {
		t.Fatalf("only %d keys found; the grep is probably broken", len(used))
	}
	for k, v := range catalog {
		if strings.TrimSpace(v) == "" {
			t.Errorf("en.json: %q is empty", k)
		}
	}
}

// TestThePanelIsInstallable checks what a browser reads to install it:
// the manifest at the root, every icon it names actually served as what
// it says it is, and the worker at the root with its scope allowed. A
// manifest that names a missing icon fails installation with nothing
// visible on the page.
func TestThePanelIsInstallable(t *testing.T) {
	h := Handler()
	rec := get(t, h, "/manifest.webmanifest")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m struct {
		Name, StartURL, Display string
		Icons                   []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		r := get(t, h, ic.Src)
		if r.Code != 200 || r.Header().Get("Content-Type") != ic.Type {
			t.Errorf("icon %s: %d %q, the manifest says %s", ic.Src, r.Code, r.Header().Get("Content-Type"), ic.Type)
		}
		sizes[ic.Sizes] = true
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("installation wants 192 and 512 icons, got %v", sizes)
	}
	rec = get(t, h, "/sw.js")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("worker: %d %v", rec.Code, rec.Header())
	}
	if shell := get(t, h, "/").Body.String(); !strings.Contains(shell, `rel="manifest" href="/manifest.webmanifest"`) {
		t.Fatal("the shell does not link the manifest")
	}
}

// TestEveryCatalogSaysTheSameThings keeps the other languages in step
// with en.json: the same keys, no empty value, and the same {placeholders}
// in each string. A key missing in es.json would show as the key itself,
// and a placeholder translated by mistake would show as "{domain}" to the
// operator; both only on screen, never in an error.
func TestEveryCatalogSaysTheSameThings(t *testing.T) {
	read := func(name string) map[string]string {
		data, err := fs.ReadFile(FS, "i18n/"+name)
		if err != nil {
			t.Fatal(err)
		}
		var c map[string]string
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return c
	}
	en := read("en.json")
	entries, err := fs.ReadDir(FS, "i18n")
	if err != nil {
		t.Fatal(err)
	}
	holes := regexp.MustCompile(`\{\w+\}`)
	others := 0
	for _, e := range entries {
		if e.Name() == "en.json" || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		others++
		c := read(e.Name())
		for k, v := range en {
			tr, ok := c[k]
			switch {
			case !ok:
				t.Errorf("%s: %q is missing", e.Name(), k)
			case strings.TrimSpace(tr) == "":
				t.Errorf("%s: %q is empty", e.Name(), k)
			default:
				a, b := holes.FindAllString(v, -1), holes.FindAllString(tr, -1)
				sort.Strings(a)
				sort.Strings(b)
				if strings.Join(a, ",") != strings.Join(b, ",") {
					t.Errorf("%s: %q has %v, en.json has %v", e.Name(), k, b, a)
				}
			}
		}
		for k := range c {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: %q is not in en.json", e.Name(), k)
			}
		}
	}
	if others == 0 {
		t.Fatal("no catalogue besides en.json")
	}
	// The list the panel offers names every catalogue there is.
	i18n, _ := fs.ReadFile(FS, "lib/i18n.js")
	for _, e := range entries {
		code := strings.TrimSuffix(e.Name(), ".json")
		if !strings.Contains(string(i18n), `"`+code+`"`) {
			t.Errorf("lib/i18n.js does not offer %s", code)
		}
	}
}

// A move's timeline names each step with site.moveStep.<step>, and a step
// without its line showed as that key: the workers' export and import did,
// from the day they were added to the mover.
func TestEveryMoveStepHasItsLine(t *testing.T) {
	data, err := fs.ReadFile(FS, "i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var en map[string]string
	if err := json.Unmarshal(data, &en); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{relocate.StepCheck, relocate.StepCopy, relocate.StepInstall, relocate.StepStop, relocate.StepFinal, relocate.StepExport, relocate.StepImport, relocate.StepCommit, relocate.StepStart, relocate.StepRepair, relocate.StepDNS, relocate.StepCleanup, relocate.StepRestore, relocate.StepDone} {
		if en["site.moveStep."+step] == "" {
			t.Errorf("site.moveStep.%s is missing", step)
		}
	}
}

// TestRandomSiteNamesFitInOneLabel reads the word list the panel draws
// random site names from. The longest name it can offer is MAX_WORDS of
// the longest words with hyphens between, and that has to be one valid
// DNS label: 63 characters at most, a to z and hyphens. A word that
// breaks it would make a name the panel happily fills in and the site
// that can never be reached. Node is not here, so the list is grepped.
func TestRandomSiteNamesFitInOneLabel(t *testing.T) {
	data, err := fs.ReadFile(FS, "lib/words.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	m := regexp.MustCompile(`MAX_WORDS = (\d+);`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("no MAX_WORDS in words.js")
	}
	most, _ := strconv.Atoi(m[1])
	start := strings.Index(src, "const WORDS = [")
	end := strings.Index(src[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("no WORDS list in words.js")
	}
	words := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(src[start:start+end], -1)
	if len(words) < 256 {
		t.Fatalf("only %d words: a name of a few would be easy to guess", len(words))
	}
	seen, longest := map[string]bool{}, 0
	valid := regexp.MustCompile(`^[a-z]+$`)
	for _, w := range words {
		if !valid.MatchString(w[1]) {
			t.Errorf("%q is not lower-case a to z", w[1])
		}
		if seen[w[1]] {
			t.Errorf("%q is in the list twice", w[1])
		}
		seen[w[1]] = true
		longest = max(longest, len(w[1]))
	}
	if n := most*longest + most - 1; n > 63 {
		t.Errorf("%d words of %d letters make a %d-character label; DNS allows 63", most, longest, n)
	}
}

// The whole panel, as a browser that takes gzip downloads it, against as
// it is written: every text file smaller, each with a tag of its own, the
// same bytes once unpacked, and the pictures left alone.
func TestThePanelsTextGoesOutCompressed(t *testing.T) {
	h := Handler()
	plain, packed := 0, 0
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "index.html" {
			return err
		}
		url := AssetPrefix + p
		raw := get(t, h, url)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Accept-Encoding", "gzip, br")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || raw.Code != 200 {
			t.Fatalf("%s: %d and %d", p, raw.Code, rec.Code)
		}
		plain += raw.Body.Len()
		packed += rec.Body.Len()
		if raw.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s: compressed for a client that did not ask", p)
		}
		if rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("%s: Content-Length %s for %d bytes", p, rec.Header().Get("Content-Length"), rec.Body.Len())
		}
		if rec.Header().Get("Content-Encoding") != "gzip" {
			if strings.HasSuffix(p, ".js") && raw.Body.Len() > 2048 {
				t.Errorf("%s: %d bytes of script not compressed", p, raw.Body.Len())
			}
			return nil
		}
		if strings.HasSuffix(p, ".png") {
			t.Errorf("%s: a picture was compressed", p)
		}
		if rec.Header().Get("ETag") == raw.Header().Get("ETag") || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: one tag for two bodies, or no Vary: %v", p, rec.Header())
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if out, _ := io.ReadAll(zr); string(out) != raw.Body.String() {
			t.Errorf("%s: not the same file once unpacked", p)
		}
		// And its own tag saves the download, as the plain one's does.
		again := httptest.NewRequest(http.MethodGet, url, nil)
		again.Header.Set("Accept-Encoding", "gzip")
		again.Header.Set("If-None-Match", rec.Header().Get("ETag"))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, again)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("%s: %d with its own tag", p, rec.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if packed > plain/2 {
		t.Errorf("the panel is %d bytes compressed of %d", packed, plain)
	}
	t.Logf("the panel: %d bytes as written, %d compressed", plain, packed)
}

// The shell points at this build's own prefix, where a file is kept for
// good; the same file by its plain address, or under another build's
// prefix, is revalidated; and every module the shell announces is there.
func TestThisBuildsFilesAreKeptForGoodUnderItsOwnPrefix(t *testing.T) {
	h := Handler()
	shell := get(t, h, "/sites/example.com")
	prefix := AssetPrefix + "~" + Version() + "/"
	body := shell.Body.String()
	if shell.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("the shell: Cache-Control %q", shell.Header().Get("Cache-Control"))
	}
	for _, want := range []string{`<script type="module" src="` + prefix + `app.js">`, `<link rel="stylesheet" href="` + prefix + `app.css">`} {
		if !strings.Contains(body, want) {
			t.Errorf("the shell lacks %s", want)
		}
	}
	links := regexp.MustCompile(`<link rel="modulepreload" href="([^"]+)">`).FindAllStringSubmatch(body, -1)
	if len(links) < 50 {
		t.Fatalf("%d modules announced", len(links))
	}
	for _, l := range links {
		rec := get(t, h, l[1])
		if rec.Code != 200 || !strings.HasPrefix(l[1], prefix) || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: %d %s", l[1], rec.Code, rec.Header().Get("Content-Type"))
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q", l[1], cc)
		}
	}
	// Every module a file imports is announced: one that is not would be
	// found a round trip later, which is what the list is there to save.
	announced := map[string]bool{prefix + "app.js": true}
	for _, l := range links {
		announced[l[1]] = true
	}
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") && p != "sw.js" && !announced[prefix+p] {
			t.Errorf("%s is not announced", p)
		}
		return err
	})
	plain := get(t, h, "/assets/app.js")
	other := get(t, h, "/assets/~000000000000/app.js")
	mine := get(t, h, prefix+"app.js")
	if plain.Body.String() != mine.Body.String() || other.Body.String() != mine.Body.String() {
		t.Error("one file, three bodies")
	}
	if plain.Header().Get("Cache-Control") != "no-cache" || other.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("kept for good outside this build's prefix: %q, %q", plain.Header().Get("Cache-Control"), other.Header().Get("Cache-Control"))
	}
	for _, p := range []string{prefix + "index.html", prefix, AssetPrefix + "~" + Version(), prefix + "missing.js"} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
