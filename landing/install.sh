#!/bin/sh
# Installs the flopwire CLI from a GitHub release.
#
#   curl -fsSL https://flopwire.com/install.sh | sh
#
# Picks the archive for this OS and CPU, verifies it against the release's
# SHA256SUMS, and puts `flopwire` in ~/.local/bin. Re-run it to upgrade: a
# running launchd device agent is restarted on the new binary.
#
#   FLOPWIRE_VERSION=v0.2.0      install that release instead of the latest
#   FLOPWIRE_INSTALL_DIR=/path   install there instead of ~/.local/bin
#   FLOPWIRE_NO_RESTART=1        leave a running device agent alone
#
# Served from the flopwire repository: landing/install.sh.
set -eu

fail() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

main() {

  repo="flopwire/flopwire"
  base="${FLOPWIRE_DOWNLOAD_BASE:-https://github.com/$repo/releases}"
  version="${FLOPWIRE_VERSION:-latest}"
  dir="${FLOPWIRE_INSTALL_DIR:-$HOME/.local/bin}"

  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail "unsupported OS $(uname -s): releases cover macOS and Linux" ;;
  esac
  case "$(uname -m)" in
    arm64 | aarch64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) fail "unsupported CPU $(uname -m): releases cover arm64 and amd64" ;;
  esac

  if [ "$version" = latest ]; then
    url="$base/latest/download"
  else
    url="$base/download/$version"
  fi
  name="flopwire_${os}_${arch}.tar.gz"

  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
  else
    fail "curl or wget is required"
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    digest() { sha256sum "$1" | cut -d' ' -f1; }
  elif command -v shasum >/dev/null 2>&1; then
    digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
  else
    fail "sha256sum or shasum is required"
  fi

  tmp=$(mktemp -d "${TMPDIR:-/tmp}/flopwire-install.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT
  trap 'exit 130' HUP INT TERM

  fetch "$url/$name" "$tmp/$name" || fail "download failed: $url/$name"
  fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || fail "download failed: $url/SHA256SUMS"
  want=$(grep " $name\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
  [ -n "$want" ] || fail "SHA256SUMS has no entry for $name"
  got=$(digest "$tmp/$name")
  [ "$got" = "$want" ] || fail "checksum mismatch for $name: got $got, want $want"

  tar -xzf "$tmp/$name" -C "$tmp" flopwire
  chmod 0755 "$tmp/flopwire"
  "$tmp/flopwire" version >/dev/null </dev/null || fail "the downloaded binary does not run on this machine"

  mkdir -p "$dir"
  # Stage next to the target so the final step is a rename on one
  # filesystem: a running agent or hook keeps its old binary, and no hook
  # ever sees a half-written one.
  staged=$(mktemp "$dir/.flopwire.XXXXXX")
  cp "$tmp/flopwire" "$staged"
  chmod 0755 "$staged"
  mv -f "$staged" "$dir/flopwire"
  installed=$("$dir/flopwire" version </dev/null)
  printf 'installed flopwire %s to %s/flopwire\n' "$installed" "$dir"

  # Restart a launchd device agent so it runs the new binary, if it runs
  # this one.
  if [ -z "${FLOPWIRE_NO_RESTART:-}" ] && [ "$os" = darwin ] &&
    command -v launchctl >/dev/null 2>&1 &&
    svc=$(launchctl print "gui/$(id -u)/com.flopwire.agent" 2>/dev/null); then
    program=$(printf '%s\n' "$svc" | sed -n 's/^[[:space:]]*program = //p' | head -n 1)
    if [ -z "$program" ] || [ "$program" = "$dir/flopwire" ]; then
      launchctl kickstart -k "gui/$(id -u)/com.flopwire.agent" &&
        printf 'restarted the device agent (launchd com.flopwire.agent)\n'
    else
      printf 'the device agent (launchd com.flopwire.agent) runs %s, not the binary just installed; point it at %s/flopwire\n' "$program" "$dir"
    fi
  fi

  case ":$PATH:" in
    *":$dir:"*) ;;
    *) printf 'add %s to your PATH, then run: flopwire setup\n' "$dir" ;;
  esac
}

# Everything above is a definition, so a truncated download runs nothing.
main "$@"
