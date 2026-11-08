package sites

import (
	"fmt"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A template returns the manifest and the files a fresh site starts with.
// Every template produces something that answers on its first tick: an
// empty folder that the engine cannot start is the kind of first
// impression that makes people think the installation is broken.
type template func(domain string) (*manifest.Manifest, map[string]string)

var templates = map[string]template{
	"static":  staticTemplate,
	"node":    nodeTemplate,
	"go":      goTemplate,
	"docker":  dockerTemplate,
	"fastapi": fastapiTemplate,
	"django":  djangoTemplate,
	"php":     phpTemplate,
	"astro":   astroTemplate,
	"project": projectTemplate,
}

// Templates lists the names Create accepts.
func Templates() []string {
	return []string{"static", "node", "go", "docker", "fastapi", "django", "php", "astro", "project"}
}

// projectTemplate is a folder without a domain: nothing to serve, so
// nothing to start until a worker is written. The README says what it is
// for, because an empty project is otherwise indistinguishable from a
// broken site.
func projectTemplate(name string) (*manifest.Manifest, map[string]string) {
	return &manifest.Manifest{Version: manifest.Version}, map[string]string{
		"README.md": "# " + name + "\n\nA project without a domain: its workers, its tasks and its conversation.\nDeclare workers in site.json; nothing here answers a hostname.\n",
	}
}

func helloHTML(domain, how string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
:root{color-scheme:light dark;--fg:#1b1b1f;--bg:#f6f6f8;--mute:#6b6b75}
@media(prefers-color-scheme:dark){:root{--fg:#e8e8ec;--bg:#121216;--mute:#9a9aa6}}
html,body{height:100%%;margin:0}
body{display:grid;place-items:center;background:var(--bg);color:var(--fg);
font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px;box-sizing:border-box}
main{max-width:34rem;text-align:center}
h1{font-size:1.5rem;font-weight:600;margin:0 0 .5rem}
p{margin:0;color:var(--mute)}
code{font-family:ui-monospace,monospace}
</style></head>
<body><main><h1>%s</h1><p>This site is up and served by middleware. %s</p></main></body></html>
`, domain, domain, how)
}

func staticTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Static: &manifest.Static{Root: "public"}}
	return m, map[string]string{
		"public/index.html": helloHTML(domain, "Replace the files in <code>public/</code> with your own."),
	}
}

// nodeServer is a dependency-free server that honours PORT, so `npm start`
// works before anyone has run npm install. host is 127.0.0.1 for a plain
// process and 0.0.0.0 inside a container, where the published port
// reaches the container's own address, not its loopback.
func nodeServer(host string) string {
	return `// Minimal server; replace with your framework. PORT is set by middleware.
const http = require("http");
const port = Number(process.env.PORT) || 3000;
http.createServer((req, res) => {
  res.setHeader("Content-Type", "text/html; charset=utf-8");
  res.end(require("fs").readFileSync(__dirname + "/public/index.html"));
}).listen(port, "` + host + `", () => console.log("listening on", port));
`
}

func nodeTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: []string{"npm", "start"},
		Env:     map[string]string{"NODE_ENV": "production"},
		Health:  &manifest.Health{Path: "/"},
	}}
	pkg := fmt.Sprintf(`{
  "name": "%s",
  "private": true,
  "version": "0.1.0",
  "scripts": {
    "start": "node server.js"
  }
}
`, packageName(domain))
	return m, map[string]string{
		"package.json":      pkg,
		"server.js":         nodeServer("127.0.0.1"),
		"public/index.html": helloHTML(domain, "Edit <code>server.js</code> or change <code>scripts.start</code> in <code>package.json</code>; middleware runs <code>npm start</code> with <code>PORT</code> set."),
		"README.md": "# " + domain + "\n\nmiddleware runs `npm start` from this folder with `PORT` set and restarts\n" +
			"the site when `site.json` changes. Put your dependencies in `package.json`\n" +
			"and run `npm install` here (or add it to `deploy.github.build`).\n",
	}
}

// packageName makes a name npm accepts out of a domain.
func packageName(domain string) string {
	return strings.ReplaceAll(domain, ".", "-")
}

const goMain = `package main

import (
	"fmt"
	"net/http"
	"os"
)

// PORT is set by middleware; the loopback bind keeps the instance private.
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	http.Handle("/", http.FileServer(http.Dir("public")))
	fmt.Println("listening on", port)
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

func goTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: []string{"go", "run", "."},
		// go run compiles before it listens; the default minute is short
		// for a cold module cache.
		Health: &manifest.Health{Path: "/", Timeout: manifest.Duration(5 * time.Minute)},
	}}
	return m, map[string]string{
		"go.mod":            "module " + packageName(domain) + "\n\ngo 1.22\n",
		"main.go":           goMain,
		"public/index.html": helloHTML(domain, "Edit <code>main.go</code>; middleware runs <code>go run .</code> with <code>PORT</code> set."),
	}
}

// dockerTemplate builds from a Dockerfile in the folder. The folder is
// mounted at /site inside the container, so the image only needs a
// runtime and the command is the same one the node template uses.
func dockerTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: []string{"node", "server.js"},
		Runtime: "docker",
		Docker:  &manifest.Docker{Build: "Dockerfile"},
		Health:  &manifest.Health{Path: "/"},
	}}
	return m, map[string]string{
		"Dockerfile":        "FROM node:22-alpine\nWORKDIR /site\n# The site folder is bind-mounted at /site by middleware; copy nothing.\n",
		"server.js":         nodeServer("0.0.0.0"),
		"public/index.html": helloHTML(domain, "Edit the <code>Dockerfile</code> and <code>run.command</code> in <code>site.json</code>; middleware builds the image and runs it with <code>PORT</code> set."),
	}
}
