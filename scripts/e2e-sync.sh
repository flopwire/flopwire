#!/bin/sh
# Two-device end-to-end sync test (spec §11 B4).
#
# Brings up the server with Compose (Postgres, MinIO, flopwire) bound to this
# machine's LAN address, so the simulated devices reach it the way a second
# laptop would. Falls back to 127.0.0.1 when that bind fails. The server
# speaks TLS with its self-signed certificate; the devices pin its
# fingerprint through the invite (D13). Then runs internal/e2e
# (FLOPWIRE_E2E=1): two `flopwire agent run` processes with separate homes,
# credentials, indexes and sync state, and the scenarios.
#
# Environment:
#   FLOPWIRE_E2E_BIND   address to publish the server on (default: this
#                     machine's primary IPv4 address, else 127.0.0.1)
#   FLOPWIRE_E2E_KEEP=1 keep the work directory and the Compose project
#   FLOPWIRE_E2E_IMAGE  use this prebuilt server image instead of building
#                     one (CI builds it with a layer cache first)
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$repo"

project="flopwire-e2esync-$$"
root=$(mktemp -d "${TMPDIR:-/tmp}/flopwire-e2esync.XXXXXX")
base=$((20000 + ($$ % 2000) * 4))
server_port=$base
pg_port=$((base + 1))
minio_port=$((base + 2))
restore_port=$((base + 3))

export POSTGRES_PASSWORD="flopwire-e2e-postgres"
export MINIO_ROOT_PASSWORD="flopwire-e2e-minio-secret"
export COMPOSE_PROJECT_NAME="$project"
export FLOPWIRE_PORT="$server_port" POSTGRES_DEV_PORT="$pg_port" MINIO_DEV_PORT="$minio_port"

files="-f compose.yaml -f compose.dev.yaml"
if [ -n "${FLOPWIRE_E2E_IMAGE:-}" ]; then
  printf 'services:\n  flopwire:\n    image: %s\n' "$FLOPWIRE_E2E_IMAGE" >"$root/compose.image.yaml"
  files="$files -f $root/compose.image.yaml"
fi
compose() { docker compose -p "$project" $files "$@"; }

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    compose logs --no-color --tail 200 flopwire >&2 || true
  fi
  if [ "${FLOPWIRE_E2E_KEEP:-}" = 1 ]; then
    printf 'kept %s and Compose project %s\n' "$root" "$project" >&2
  else
    docker rm -f "$project-restored" >/dev/null 2>&1 || true
    # The backup is written by the container user (uid 10001); on Linux
    # only that user can remove it.
    if [ -d "$root/backup" ]; then
      compose run --rm --no-deps --entrypoint rm -v "$root/backup:/backup" flopwire -rf /backup/b1 >/dev/null 2>&1 || true
    fi
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$root"
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

# This machine's primary non-loopback IPv4 address (no VPN assumed).
lan_ip() {
  ip=""
  if command -v ipconfig >/dev/null 2>&1; then
    iface=$(route -n get default 2>/dev/null | awk '/interface:/ { print $2 }')
    [ -n "$iface" ] && ip=$(ipconfig getifaddr "$iface" 2>/dev/null || true)
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
    ip=$(hostname -I 2>/dev/null | awk '{ print $1 }' || true)
  fi
  printf '%s' "$ip"
}

bind=${FLOPWIRE_E2E_BIND:-$(lan_ip)}
bind=${bind:-127.0.0.1}

if [ -z "${FLOPWIRE_E2E_IMAGE:-}" ]; then
  compose build flopwire
fi
if ! FLOPWIRE_BIND="$bind" compose up -d --wait --wait-timeout 180; then
  if [ "$bind" = 127.0.0.1 ]; then
    exit 1
  fi
  printf 'e2e-sync: binding %s failed; falling back to 127.0.0.1\n' "$bind" >&2
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  bind=127.0.0.1
  FLOPWIRE_BIND="$bind" compose up -d --wait --wait-timeout 180
fi
export FLOPWIRE_BIND="$bind"
server="https://$bind:$server_port"
fingerprint=$(compose exec -T flopwire flopwire fingerprint | tr -d '\r')
printf 'e2e-sync: server %s pinned %s (project %s, work dir %s)\n' "$server" "$fingerprint" "$project" "$root"

go build -trimpath -o "$root/flopwire" ./cmd/flopwire

export FLOPWIRE_E2E=1
export FLOPWIRE_E2E_BIN="$root/flopwire"
export FLOPWIRE_E2E_SERVER="$server"
export FLOPWIRE_E2E_FINGERPRINT="$fingerprint"
export FLOPWIRE_E2E_DATABASE_URL="postgres://flopwire:$POSTGRES_PASSWORD@127.0.0.1:$pg_port/flopwire?sslmode=disable"
export FLOPWIRE_E2E_POSTGRES_PASSWORD="$POSTGRES_PASSWORD"
export FLOPWIRE_E2E_COMPOSE="docker compose -p $project $files"
export FLOPWIRE_E2E_PROJECT="$project"
export FLOPWIRE_E2E_S3_ENDPOINT="127.0.0.1:$minio_port"
export FLOPWIRE_E2E_S3_USER="flopwire"
export FLOPWIRE_E2E_S3_PASSWORD="$MINIO_ROOT_PASSWORD"
export FLOPWIRE_E2E_ROOT="$root"
export FLOPWIRE_E2E_RESTORE_PORT="$restore_port"
export FLOPWIRE_E2E_REPORT="${FLOPWIRE_E2E_REPORT:-$root/report.md}"

go test -count=1 -v -timeout 20m -run TestTwoDeviceSync ./internal/e2e/
printf 'e2e-sync: server bound to %s\n' "$bind"
cat "$FLOPWIRE_E2E_REPORT"
