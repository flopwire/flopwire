#!/bin/sh
# Exercises landing/install.sh against a fake release served from a local
# directory: a good install, a checksum mismatch, and a pinned version.
set -eu
repo=$(cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/flopwire-install-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

case "$(uname -s)" in Darwin) os=darwin ;; *) os=linux ;; esac
case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
name="flopwire_${os}_${arch}.tar.gz"

# A release holds a fake flopwire that prints its version.
release() { # dir version
  mkdir -p "$1/src"
  printf '#!/bin/sh\necho %s\n' "$2" >"$1/src/flopwire"
  chmod 0755 "$1/src/flopwire"
  tar -czf "$1/$name" -C "$1/src" flopwire
  (cd "$1" && { sha256sum "$name" 2>/dev/null || shasum -a 256 "$name"; } >SHA256SUMS)
}
release "$tmp/latest/download" v9.9.9
release "$tmp/download/v9.9.8" v9.9.8

# curl -o with a file:// URL serves the fake release.
export FLOPWIRE_DOWNLOAD_BASE="file://$tmp" FLOPWIRE_INSTALL_DIR="$tmp/bin" HOME="$tmp/home"
mkdir -p "$HOME"

out=$(sh "$repo/landing/install.sh")
case "$out" in *"installed flopwire v9.9.9 to $tmp/bin/flopwire"*) ;; *) echo "latest: unexpected output: $out" >&2; exit 1 ;; esac
test "$("$tmp/bin/flopwire")" = v9.9.9

out=$(FLOPWIRE_VERSION=v9.9.8 sh "$repo/landing/install.sh")
case "$out" in *"installed flopwire v9.9.8"*) ;; *) echo "pinned: unexpected output: $out" >&2; exit 1 ;; esac
test "$("$tmp/bin/flopwire")" = v9.9.8

printf 'tampered' >>"$tmp/latest/download/$name"
if err=$(sh "$repo/landing/install.sh" 2>&1); then
  echo "tampered archive installed: $err" >&2
  exit 1
fi
case "$err" in *"checksum mismatch"*) ;; *) echo "tampered: unexpected error: $err" >&2; exit 1 ;; esac
test "$("$tmp/bin/flopwire")" = v9.9.8

echo "install_test: ok"
