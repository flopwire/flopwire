#!/bin/sh
set -eu

e2e_root=$(mktemp -d /tmp/flopwire-e2e-XXXXXX)
e2e_project="flopwire-e2e-$$"
e2e_postgres_password="flopwire-development-only"
e2e_minio_password="flopwire-development-secret"
e2e_postgres_image="postgres:16"
e2e_minio_image="minio/minio:latest"
e2e_postgres_port="${FLOPWIRE_E2E_POSTGRES_PORT:-55440}"
e2e_minio_port="${FLOPWIRE_E2E_MINIO_PORT:-59081}"
e2e_database="postgres://flopwire:$e2e_postgres_password@127.0.0.1:$e2e_postgres_port/flopwire?sslmode=disable"
e2e_api=http://127.0.0.1:58080
e2e_server_pid=""

compose() {
  POSTGRES_PASSWORD="$e2e_postgres_password" MINIO_ROOT_PASSWORD="$e2e_minio_password" \
    POSTGRES_IMAGE="$e2e_postgres_image" MINIO_IMAGE="$e2e_minio_image" \
    POSTGRES_DEV_PORT="$e2e_postgres_port" MINIO_DEV_PORT="$e2e_minio_port" \
    COMPOSE_PROJECT_NAME="$e2e_project" \
    docker compose -f compose.yaml -f compose.dev.yaml "$@"
}

cleanup() {
  if [ -n "$e2e_server_pid" ]; then
    kill -TERM "$e2e_server_pid" 2>/dev/null || true
    wait "$e2e_server_pid" 2>/dev/null || true
  fi
  compose down --volumes >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

compose up -d --wait postgres minio

go build -trimpath -o "$e2e_root/flopwire" ./cmd/flopwire

DATABASE_URL="$e2e_database" \
S3_ENDPOINT="127.0.0.1:$e2e_minio_port" \
S3_ACCESS_KEY='flopwire' \
S3_SECRET_KEY="$e2e_minio_password" \
S3_BUCKET='flopwire' \
S3_SECURE='false' \
  "$e2e_root/flopwire" serve --addr 127.0.0.1:58080 --tls=off >"$e2e_root/server.log" 2>&1 &
e2e_server_pid=$!

e2e_ready=false
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  if curl -fsS "$e2e_api/healthz" >/dev/null 2>&1; then e2e_ready=true; break; fi
  sleep 1
done
if [ "$e2e_ready" != true ]; then
  sed -n '1,120p' "$e2e_root/server.log"
  exit 1
fi

status() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }

# The network API cannot bootstrap; the CLI writes the first admin directly.
test "$(status -X POST "$e2e_api/v1/bootstrap")" = 404
export FLOPWIRE_CONFIG="$e2e_root/admin.json"
printf 'correct horse battery staple\ncorrect horse battery staple\n' |
  DATABASE_URL="$e2e_database" "$e2e_root/flopwire" bootstrap --server "$e2e_api" \
    --name Admin --email admin@example.test >"$e2e_root/bootstrap.json"

# An admin's device credential carries no administrative authority.
"$e2e_root/flopwire" enroll --name admin-collector --platform linux-amd64 >"$e2e_root/admin-enroll.json"
e2e_admin_device_token=$(jq -r .token "$FLOPWIRE_CONFIG")
test "$(status -X POST "$e2e_api/v1/admin/invites" -H "Authorization: Bearer $e2e_admin_device_token" \
  -H 'Content-Type: application/json' --data '{"email":"forbidden@example.test","role":"member"}')" = 403
"$e2e_root/flopwire" invite --email member@example.test >"$e2e_root/invite.json"
e2e_invite_code=$(jq -r .code "$e2e_root/invite.json")

export FLOPWIRE_CONFIG="$e2e_root/member.json"
printf 'another correct horse password\nanother correct horse password\n' |
  "$e2e_root/flopwire" claim --server "$e2e_api" --code "$e2e_invite_code" --name Member >"$e2e_root/claim.json"
"$e2e_root/flopwire" enroll --name e2e-mac --platform linux-amd64 >"$e2e_root/enroll.json"
e2e_old_member_token=$(jq -r .token "$FLOPWIRE_CONFIG")
"$e2e_root/flopwire" rotate-device >"$e2e_root/rotate.json"
e2e_member_token=$(jq -r .token "$FLOPWIRE_CONFIG")
test "$e2e_member_token" != "$e2e_old_member_token"
test "$(status "$e2e_api/v1/policy" -H "Authorization: Bearer $e2e_old_member_token")" = 401
test "$(status "$e2e_api/v1/policy" -H "Authorization: Bearer $e2e_member_token")" = 200

# Upload and search are rebuilt on chunks and message rows (B2/B3). Until
# then the endpoints answer 501.
test "$(status "$e2e_api/v1/search?q=cobalt" -H "Authorization: Bearer $e2e_member_token")" = 501

if command -v pg_dump >/dev/null 2>&1; then
  DATABASE_URL="$e2e_database" \
  S3_ENDPOINT="127.0.0.1:$e2e_minio_port" \
  S3_ACCESS_KEY='flopwire' \
  S3_SECRET_KEY="$e2e_minio_password" \
  S3_BUCKET='flopwire' \
  S3_SECURE='false' \
    "$e2e_root/flopwire" backup --encrypted-destination --output "$e2e_root/backup" >"$e2e_root/backup.json"
  "$e2e_root/flopwire" backup-verify --input "$e2e_root/backup" >"$e2e_root/backup-verify.json"
  jq -e '.verified == true and .objects == 0' "$e2e_root/backup-verify.json" >/dev/null
fi

export FLOPWIRE_CONFIG="$e2e_root/admin.json"
e2e_admin_session=$(jq -r .session_token "$FLOPWIRE_CONFIG")
e2e_status=$(curl -fsS "$e2e_api/v1/admin/status" -H "Authorization: Bearer $e2e_admin_session")
printf '%s' "$e2e_status" | jq -e '.storage.used_bytes == 0 and .audit.event_count > 0' >/dev/null
if command -v pg_dump >/dev/null 2>&1; then
  printf '%s' "$e2e_status" | jq -e '.last_backup.action == "backup.complete"' >/dev/null
fi

printf 'flopwire E2E passed\n'
