#!/bin/sh
set -eu

tag=${1-}
pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'

printf '%s\n' "$tag" | grep -Eq "$pattern"

without_build=${tag%%+*}
case "$without_build" in
  *-*)
    prerelease=${without_build#*-}
    old_ifs=$IFS
    IFS=.
    for identifier in $prerelease; do
      case "$identifier" in
        *[!0-9]*) ;;
        0) ;;
        0*) exit 1 ;;
      esac
    done
    IFS=$old_ifs
    ;;
esac
