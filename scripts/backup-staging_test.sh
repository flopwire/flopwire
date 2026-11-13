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
# Check the capacity bound without allocating an oversized file. The fake
# filesystem has one byte less than the 64 MiB scratch request.
mkdir "$root/fake-bin"
printf '#!/bin/sh\nprintf "Avail\\n67108863\\n"\n' >"$root/fake-bin/df"
chmod +x "$root/fake-bin/df"
if PATH="$root/fake-bin:$PATH" FLOPWIRE_BACKUP_STATE_DIR="$root/no-room" \
    FLOPWIRE_BACKUP_STAGING_DIR="$root/no-room-mount" FLOPWIRE_BACKUP_MIN_FREE_BYTES=0 \
    sh "$script" start 2>/dev/null; then
    echo 'insufficient headroom was accepted' >&2
    exit 1
fi
test ! -e "$root/no-room/staging.img"
printf '#!/bin/sh\nprintf "Avail\\n134217728\\n"\n' >"$root/fake-bin/df"
if PATH="$root/fake-bin:$PATH" FLOPWIRE_BACKUP_STATE_DIR="$root/no-reserve" \
    FLOPWIRE_BACKUP_STAGING_DIR="$root/no-reserve-mount" FLOPWIRE_BACKUP_MIN_FREE_BYTES=67108865 \
    sh "$script" start 2>/dev/null; then
    echo 'insufficient server reserve was accepted' >&2
    exit 1
fi
test ! -e "$root/no-reserve/staging.img"
# A failed allocation must leave neither a partial image nor a stale marker
# that could authorize formatting the failed allocation on the next run.
printf '#!/bin/sh\nprintf partial >"$3"\nexit 1\n' >"$root/fake-bin/fallocate"
chmod +x "$root/fake-bin/fallocate"
mkdir "$root/allocation-failed"
printf '%s\n' 'flopwire encrypted scratch v1' >"$root/allocation-failed/staging.managed"
if PATH="$root/fake-bin:$PATH" FLOPWIRE_BACKUP_STATE_DIR="$root/allocation-failed" \
    FLOPWIRE_BACKUP_STAGING_DIR="$root/allocation-failed-mount" FLOPWIRE_BACKUP_MIN_FREE_BYTES=0 \
    sh "$script" start 2>/dev/null; then
    echo 'failed allocation was accepted' >&2
    exit 1
fi
test ! -e "$root/allocation-failed/staging.img"
test ! -e "$root/allocation-failed/staging.managed"
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
