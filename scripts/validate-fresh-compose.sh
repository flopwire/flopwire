#!/bin/sh
set -eu

image=${1:?usage: validate-fresh-compose.sh IMAGE PLATFORM EXPECTED_VERSION}
platform=${2:?usage: validate-fresh-compose.sh IMAGE PLATFORM EXPECTED_VERSION}
expected_version=${3:?usage: validate-fresh-compose.sh IMAGE PLATFORM EXPECTED_VERSION}
repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
project="flopwirefresh$$"
tmp=$(mktemp -d)
override="$tmp/compose.verify.yaml"

export FLOPWIRE_TEST_IMAGE=$image
export FLOPWIRE_TEST_PLATFORM=$platform
export POSTGRES_PASSWORD=flopwire-fresh-postgres
export MINIO_ROOT_PASSWORD=flopwire-fresh-minio
export FLOPWIRE_PORT=${FLOPWIRE_TEST_PORT:-$((18000 + ($$ % 1000)))}

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" ps >&2 || true
    docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" logs --no-color >&2 || true
  fi
  docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

cat >"$override" <<'EOF'
services:
  flopwire:
    image: ${FLOPWIRE_TEST_IMAGE}
    platform: ${FLOPWIRE_TEST_PLATFORM}
    pull_policy: never
EOF

# The unique project name guarantees new named volumes. The preflight removal
# also makes a reused PID safe after an interrupted local run.
docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" down --volumes --remove-orphans >/dev/null 2>&1 || true
docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" config --quiet
docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" up -d --no-build --wait --wait-timeout 120

actual_version=$(docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T flopwire flopwire version | tr -d '\r')
test "$actual_version" = "$expected_version"

# The runtime is the non-root flopwire user (uid 10001, gid 999).
identity=$(docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T flopwire id -u):$(docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T flopwire id -g)
test "$(printf '%s' "$identity" | tr -d '\r')" = "10001:999"

expected_migrations=$(find "$repo/migrations" -maxdepth 1 -name '*.sql' | wc -l | tr -d ' ')
applied_migrations=$(docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T postgres \
  psql -U flopwire -d flopwire -Atc 'SELECT count(*) FROM flopwire_schema_migrations' | tr -d '\r')
test "$applied_migrations" = "$expected_migrations"

docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T flopwire \
  flopwire healthcheck

# The server speaks TLS with a persisted self-signed certificate (D13).
fingerprint=$(docker compose -p "$project" -f "$repo/compose.yaml" -f "$override" exec -T flopwire flopwire fingerprint | tr -d '\r')
case "$fingerprint" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "bad fingerprint: $fingerprint" >&2; exit 1 ;; esac
