#!/bin/sh
# A real encryption/mount test with synthetic bytes, never production paths.
set -eu
if [ "$(uname -s)" != Linux ] || [ "$(id -u)" != 0 ]; then
    echo 'backup staging integration skipped: Linux root required'
    exit 0
fi
root=$(mktemp -d /tmp/flopwire-staging-test-XXXXXX)
export FLOPWIRE_BACKUP_STATE_DIR=$root/state
export FLOPWIRE_BACKUP_STAGING_DIR=$root/mount
export FLOPWIRE_BACKUP_MAPPING=flopwire-staging-test-$$
export FLOPWIRE_BACKUP_STAGING_BYTES=67108864
script=$(dirname "$0")/../deploy/systemd/prepare-backup-staging.sh
cleanup() {
    sh "$script" stop
    rm -rf "$root"
}
trap cleanup EXIT
sh "$script" start
printf synthetic >"$FLOPWIRE_BACKUP_STAGING_DIR/sentinel"
printf preserved >"$FLOPWIRE_BACKUP_STATE_DIR/status-test"
sh "$script" start
test -f "$FLOPWIRE_BACKUP_STAGING_DIR/sentinel"
sh "$script" stop
sh "$script" start
test ! -e "$FLOPWIRE_BACKUP_STAGING_DIR/sentinel"
test -f "$FLOPWIRE_BACKUP_STATE_DIR/status-test"
sh "$script" stop
# A file the script did not create must never be reformatted.
rm "$FLOPWIRE_BACKUP_STATE_DIR/staging.managed"
if sh "$script" start 2>/dev/null; then
    echo 'unowned image was accepted' >&2
    exit 1
fi
echo 'encrypted staging lifecycle passed'
