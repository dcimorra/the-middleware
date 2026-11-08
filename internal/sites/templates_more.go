package sites

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// The templates below need a runtime the machine may not have (Python
// with packages, PHP), so they run in a container built from the
// Dockerfile they bring: the packages are installed into the image at
// build time and the site folder is mounted over the working directory,
// so editing the code needs no rebuild and changing requirements.txt
// does (the image is tagged with the folder's revision).
//
// A first build downloads a base image and installs packages, which on a
// small machine takes minutes; the default minute of health would call a
// healthy first start a failure.
const firstBuild = manifest.Duration(10 * time.Minute)

// pythonDockerfile installs requirements.txt into the image. The copy of
// requirements.txt alone, before anything else, is what lets docker reuse
// the layer when only the code changed.
const pythonDockerfile = `FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt
# The site folder is mounted at its own path by middleware and is the
# working directory; copy nothing else.
`

// The container is reached only through the house, on a port published
// on loopback, so whoever sends X-Forwarded-* is the house: '*' is the
// house's address as seen from inside, which changes with the network.
const fastapiMain = `from fastapi import FastAPI
from fastapi.responses import HTMLResponse
from pathlib import Path

app = FastAPI()
page = Path(__file__).with_name("index.html")


@app.get("/", response_class=HTMLResponse)
def index():
    return page.read_text()


@app.get("/api/health")
def health():
    return {"ok": True}
`

func fastapiTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: []string{"sh", "-c", `exec uvicorn main:app --host 0.0.0.0 --port "$PORT" --proxy-headers --forwarded-allow-ips '*'`},
		Runtime: "docker",
		Docker:  &manifest.Docker{Build: "Dockerfile"},
		Health:  &manifest.Health{Path: "/api/health", Timeout: firstBuild},
	}}
	return m, map[string]string{
		"Dockerfile":       pythonDockerfile,
		"requirements.txt": "fastapi>=0.115,<1\nuvicorn[standard]>=0.30,<1\n",
		"main.py":          fastapiMain,
		"index.html":       helloHTML(domain, "Edit <code>main.py</code>; packages go in <code>requirements.txt</code>, and middleware rebuilds the image when it changes."),
		"README.md": "# " + domain + "\n\nA FastAPI app. middleware builds the `Dockerfile` (which installs\n" +
			"`requirements.txt`) and runs `uvicorn main:app` on `$PORT`. The folder is\n" +
			"mounted inside the container, so a code change needs only a restart.\n" +
			"Keep what the app writes under `data/`: a promotion from a test copy\n" +
			"leaves that folder as it is in production.\n",
	}
}

// Django's own host check is there for a server that answers any name;
// this one is reached only through the house, which routes by name before
// a request gets here, so the check would only be a second list to keep
// in step with the site's names (and the health check, which asks by
// address, would fail it).
const djangoSettings = `import os
from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent.parent
SECRET_KEY = os.environ["DJANGO_SECRET_KEY"]
DEBUG = os.environ.get("DJANGO_DEBUG") == "1"
# middleware routes by host before a request reaches this process.
ALLOWED_HOSTS = ["*"]
CSRF_TRUSTED_ORIGINS = [o for o in os.environ.get("DJANGO_TRUSTED_ORIGINS", "").split(",") if o]
SECURE_PROXY_SSL_HEADER = ("HTTP_X_FORWARDED_PROTO", "https")
USE_X_FORWARDED_HOST = True

INSTALLED_APPS = [
    "django.contrib.admin",
    "django.contrib.auth",
    "django.contrib.contenttypes",
    "django.contrib.sessions",
    "django.contrib.messages",
    "django.contrib.staticfiles",
]
MIDDLEWARE = [
    "django.middleware.security.SecurityMiddleware",
    "whitenoise.middleware.WhiteNoiseMiddleware",
    "django.contrib.sessions.middleware.SessionMiddleware",
    "django.middleware.common.CommonMiddleware",
    "django.middleware.csrf.CsrfViewMiddleware",
    "django.contrib.auth.middleware.AuthenticationMiddleware",
    "django.contrib.messages.middleware.MessageMiddleware",
    "django.middleware.clickjacking.XFrameOptionsMiddleware",
]
ROOT_URLCONF = "config.urls"
TEMPLATES = [{
    "BACKEND": "django.template.backends.django.DjangoTemplates",
    "DIRS": [BASE_DIR / "templates"],
    "APP_DIRS": True,
    "OPTIONS": {"context_processors": [
        "django.template.context_processors.request",
        "django.contrib.auth.context_processors.auth",
        "django.contrib.messages.context_processors.messages",
    ]},
}]
WSGI_APPLICATION = "config.wsgi.application"
# Under data/, which a promotion from a test copy leaves as it is.
DATABASES = {"default": {"ENGINE": "django.db.backends.sqlite3", "NAME": BASE_DIR / "data" / "db.sqlite3"}}
STATIC_URL = "static/"
STATIC_ROOT = BASE_DIR / "data" / "static"
DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"
USE_TZ = True
`

const djangoURLs = `from django.contrib import admin
from django.urls import path
from django.views.generic import TemplateView

urlpatterns = [
    path("", TemplateView.as_view(template_name="index.html")),
    path("admin/", admin.site.urls),
]
`

const djangoWSGI = `import os
from django.core.wsgi import get_wsgi_application

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")
application = get_wsgi_application()
`

const djangoManage = `#!/usr/bin/env python
import os
import sys

if __name__ == "__main__":
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")
    from django.core.management import execute_from_command_line
    execute_from_command_line(sys.argv)
`

func djangoTemplate(domain string) (*manifest.Manifest, map[string]string) {
	// migrate and collectstatic on every start: both are no-ops when
	// nothing changed, and a deploy that adds a migration would otherwise
	// start on a schema it does not have.
	start := `mkdir -p data && python manage.py migrate --noinput && python manage.py collectstatic --noinput >/dev/null && ` +
		`exec gunicorn config.wsgi --bind 0.0.0.0:"$PORT" --workers 2 --forwarded-allow-ips '*'`
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command:  []string{"sh", "-c", start},
		Runtime:  "docker",
		Docker:   &manifest.Docker{Build: "Dockerfile"},
		EnvFiles: []string{".env"},
		Env:      map[string]string{"DJANGO_TRUSTED_ORIGINS": "https://" + domain},
		Health:   &manifest.Health{Path: "/", Timeout: firstBuild},
	}}
	return m, map[string]string{
		"Dockerfile":           pythonDockerfile,
		"requirements.txt":     "Django>=5.1,<6\ngunicorn>=23,<24\nwhitenoise>=6.7,<7\n",
		"manage.py":            djangoManage,
		"config/__init__.py":   "",
		"config/settings.py":   djangoSettings,
		"config/urls.py":       djangoURLs,
		"config/wsgi.py":       djangoWSGI,
		"templates/index.html": helloHTML(domain, "Edit <code>config/urls.py</code> and <code>templates/</code>; add apps with <code>python manage.py startapp</code>."),
		// The key is made here, once, and kept in .env: a key that changed
		// on every start would sign everyone out each time. .env is never
		// served and a promotion from a test copy leaves it in place.
		".env": "DJANGO_SECRET_KEY=" + secretKey() + "\n",
		"README.md": "# " + domain + "\n\nA Django project. middleware builds the `Dockerfile` (which installs\n" +
			"`requirements.txt`), runs the migrations and serves it with gunicorn on\n" +
			"`$PORT`. The database is `data/db.sqlite3`; the secret key is in `.env`.\n" +
			"Create an admin with `middleware exec " + domain + " -- python manage.py createsuperuser`.\n",
	}
}

// PHP's own server, with workers: the official image runs Apache as root
// and drops to www-data, and a site's container gets no capabilities and
// runs as the supervisor's user, so Apache cannot start in it. The
// built-in server with PHP_CLI_SERVER_WORKERS answers several requests at
// once, which is enough for the sites this template is for; a busy one
// moves to php-fpm behind its own server.
const phpDockerfile = `FROM php:8.3-cli
RUN docker-php-ext-install pdo_mysql
# The site folder is mounted at its own path by middleware and is the
# working directory; copy nothing.
`

const phpIndex = `<?php
// PHP_CLI_SERVER_WORKERS and PORT are set by middleware.
$page = file_get_contents(__DIR__ . "/hello.html");
echo str_replace("{{php}}", htmlspecialchars(PHP_VERSION), $page);
`

func phpTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: []string{"sh", "-c", `exec php -S 0.0.0.0:"$PORT" -t public`},
		Runtime: "docker",
		Docker:  &manifest.Docker{Build: "Dockerfile"},
		Env:     map[string]string{"PHP_CLI_SERVER_WORKERS": "4"},
		Health:  &manifest.Health{Path: "/", Timeout: firstBuild},
	}}
	return m, map[string]string{
		"Dockerfile":        phpDockerfile,
		"public/index.php":  phpIndex,
		"public/hello.html": helloHTML(domain, "Running on PHP {{php}}. Edit <code>public/index.php</code>; extensions go in the <code>Dockerfile</code>."),
		"README.md": "# " + domain + "\n\nA PHP site served from `public/` by PHP's own server with\n" +
			"`PHP_CLI_SERVER_WORKERS` workers, in a container built from the `Dockerfile`.\n" +
			"Add extensions there (`docker-php-ext-install`); the image is rebuilt\n" +
			"when the folder changes.\n",
	}
}

const astroPage = `---
const title = Astro.site?.hostname ?? "%s";
---
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width,initial-scale=1" />
    <title>{title}</title>
  </head>
  <body>
    <main>
      <h1>{title}</h1>
      <p>Built by Astro and served as files by middleware.</p>
    </main>
  </body>
</html>
`

// astroTemplate serves dist/, which is what `npm run build` writes. The
// house builds nothing for a site that is not deployed from a repository,
// so dist/ starts with a page already in it: the site answers on its
// first tick, and the first build replaces it.
func astroTemplate(domain string) (*manifest.Manifest, map[string]string) {
	m := &manifest.Manifest{Version: manifest.Version, Static: &manifest.Static{Root: "dist"}}
	pkg := fmt.Sprintf(`{
  "name": "%s",
  "private": true,
  "type": "module",
  "version": "0.1.0",
  "scripts": {
    "dev": "astro dev",
    "build": "astro build"
  },
  "dependencies": {
    "astro": "^5.0.0"
  }
}
`, packageName(domain))
	return m, map[string]string{
		"package.json":          pkg,
		"astro.config.mjs":      "import { defineConfig } from \"astro/config\";\n\nexport default defineConfig({ site: \"https://" + domain + "\" });\n",
		"src/pages/index.astro": fmt.Sprintf(astroPage, domain),
		"dist/index.html":       helloHTML(domain, "Edit <code>src/pages/</code> and run <code>npm install &amp;&amp; npm run build</code>; middleware serves <code>dist/</code>."),
		".gitignore":            "node_modules/\ndist/\n.astro/\n",
		"README.md": "# " + domain + "\n\nAn Astro site, served as files from `dist/`. Build it with\n" +
			"`npm install && npm run build` (deployed from GitHub, put both in\n" +
			"`deploy.github.build`). Until the first build, `dist/` holds a\n" +
			"placeholder page.\n",
	}
}

// secretKey is 50 random characters, the length Django's own generator
// uses.
func secretKey() string {
	b := make([]byte, 38)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:50]
}
