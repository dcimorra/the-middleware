#!/bin/sh
# Bootstrap for: curl -fsSL https://raw.githubusercontent.com/dcimorra/the-middleware/main/scripts/install.sh | sh
#
# Downloads the latest release binary for this machine, verifies its sha256
# against the file published next to it, and hands over to `middleware
# install`, which does the actual work and explains every step. POSIX sh on
# purpose: the machine may have nothing but busybox.
set -eu

REPO="dcimorra/the-middleware"

# MIDDLEWARE_VERSION=v2.0.0 pins a release. Otherwise the newest release
# is asked from the API, prereleases included: GitHub's "latest" link skips
# prereleases, and while every release is an alpha it would resolve to
# nothing at all.
resolve_tag() {
  if [ -n "${MIDDLEWARE_VERSION:-}" ]; then
    printf '%s\n' "$MIDDLEWARE_VERSION"
    return
  fi
  # The newest by version, not the first listed: GitHub orders releases
  # by tag name as text, and alpha.10 came after alpha.2 and before
  # alpha.3, so the first one was alpha.9 for ever. Each tag gets a
  # sortable key: major, minor, patch, release-after-pre-release, the
  # pre-release's name and number.
  tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=100" 2>/dev/null \
    | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\(v[0-9][^"]*\)".*/\1/p' \
    | awk '{ s = $0; sub(/^v/, "", s); pre = ""; i = index(s, "-");
             if (i) { pre = substr(s, i + 1); s = substr(s, 1, i - 1) }
             split(s, a, "."); np = split(pre, p, ".");
             printf "%08d%08d%08d %d %-12s %08d %s\n", a[1], a[2], a[3], (pre == ""), p[1], p[2], $0 }' \
    | sort | tail -n 1 | awk '{ print $NF }')"
  if [ -n "$tag" ]; then
    printf '%s\n' "$tag"
  else
    printf 'latest\n'
  fi
}

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

# Everything that needs root goes through sudo from `middleware install`.
# Running the whole thing as root would make root the owner of the sites.
if [ "$(id -u)" = "0" ]; then
  die "do not run as root. Run as the user that will operate the panel; sudo is used where needed."
fi

command -v curl >/dev/null 2>&1 || die "curl is required to download the release"
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || die "sha256sum or shasum is required to verify the download"

case "$(uname -s)" in
  Linux) ;;
  *) die "middleware runs on Linux (systemd); for other systems see docs/install.md (Docker)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "no release binary for $(uname -m); build from source with scripts/build-from-source.sh" ;;
esac

NAME="middleware-linux-$ARCH"
TAG="$(resolve_tag)"
if [ "$TAG" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$TAG"
fi
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "Downloading $BASE/$NAME"
# A bar while a person watches (stderr is still the terminal when the
# script itself comes through a pipe); silent in a log.
if [ -t 2 ]; then
  curl -fL --progress-bar -o "$TMP/$NAME" "$BASE/$NAME"
else
  curl -fsSL -o "$TMP/$NAME" "$BASE/$NAME"
fi
curl -fsSL -o "$TMP/$NAME.sha256" "$BASE/$NAME.sha256"

# The .sha256 file is "<hash>  <name>", as sha256sum writes it.
WANT="$(cut -d' ' -f1 "$TMP/$NAME.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$NAME" | cut -d' ' -f1)"
else
  GOT="$(shasum -a 256 "$TMP/$NAME" | cut -d' ' -f1)"
fi
[ -n "$WANT" ] || die "empty checksum file"
[ "$WANT" = "$GOT" ] || die "checksum mismatch: expected $WANT, got $GOT. Not installing."
say "Checksum verified."

chmod 0755 "$TMP/$NAME"

# From here on the binary explains itself: it copies itself to
# /usr/local/bin and prints one line per step. Extra arguments to this
# script (for example --root /opt/middleware --no-helper) are passed through.
# Not exec'd, so the trap above removes the download afterwards.
if [ -t 0 ]; then
  "$TMP/$NAME" install "$@"
elif (: </dev/tty) 2>/dev/null; then
  # Piped through sh, stdin is the script. sudo still needs a terminal for
  # its password prompt, so reattach it; the confirmation is skipped.
  # Opened, not looked at: /dev/tty is there as a device with no terminal
  # behind it too (cloud-init, a CI job, cron), where opening it fails and
  # `set -e` ended the script here, after the download, with no word.
  "$TMP/$NAME" install --yes "$@" </dev/tty
else
  "$TMP/$NAME" install --yes "$@"
fi
