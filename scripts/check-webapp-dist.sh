#!/bin/sh
# Rebuilds the admin console and fails if internal/webapp/dist differs from
# what is committed. The committed files are what `go build`, `go install`,
# the Docker image and the release binaries embed, so every change under
# web/ must come with a rebuilt dist (`make web`).
set -eu
repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$repo"
make web >/dev/null
if ! git diff --quiet -- internal/webapp/dist ||
  [ -n "$(git ls-files --others --exclude-standard -- internal/webapp/dist)" ]; then
  git status --short -- internal/webapp/dist >&2
  echo "internal/webapp/dist is stale: run 'make web' and commit the result" >&2
  exit 1
fi
echo "internal/webapp/dist matches the web source"
