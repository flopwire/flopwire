#!/bin/sh
set -eu

for tag in v0.1.0 v1.2.3 v1.2.3-0 v1.2.3-rc.1 v1.2.3-alpha--beta v1.2.3-- v1.2.3+build--5 v1.2.3+build.5 v1.2.3-rc.1+build.5; do
  scripts/validate-release-tag.sh "$tag"
done

for tag in v1.2 v1.2.3.4 v01.2.3 v1.02.3 v1.2.03 v1.2.3- v1.2.3-01 v1.2.3-rc.01 v1.2.3-.. v1.2.3+ v1.2.3+.. release-1.2.3; do
  if scripts/validate-release-tag.sh "$tag"; then
    printf 'invalid release tag accepted: %s\n' "$tag" >&2
    exit 1
  fi
done
