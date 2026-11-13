#!/bin/sh
# Encrypted scratch space. Each boot uses a new key held only by the kernel.
set -eu
umask 077
state=${FLOPWIRE_BACKUP_STATE_DIR:-/var/lib/flopwire-backup}
staging=${FLOPWIRE_BACKUP_STAGING_DIR:-/mnt/flopwire-backup}
mapping=${FLOPWIRE_BACKUP_MAPPING:-flopwire-backup-staging}
bytes=${FLOPWIRE_BACKUP_STAGING_BYTES:-34359738368}
reserve=${FLOPWIRE_BACKUP_MIN_FREE_BYTES:-8589934592}
image=$state/staging.img
marker=$state/staging.managed
case "$state:$staging" in /*:/*) ;; *) echo 'absolute directories required' >&2; exit 1;; esac
case "$mapping" in ''|*[!a-zA-Z0-9_-]*) echo 'invalid mapper name' >&2; exit 1;; esac
case "$bytes" in ''|*[!0-9]*) echo 'invalid staging size' >&2; exit 1;; esac
case "$reserve" in ''|*[!0-9]*) echo 'invalid free-space reserve' >&2; exit 1;; esac
[ "$bytes" -ge 67108864 ]
[ "$reserve" -ge 0 ]
[ ! -L "$state" ] && [ ! -L "$staging" ] && [ ! -L "$image" ] && [ ! -L "$marker" ]
mkdir -p "$state" "$staging"
chmod 700 "$state" "$staging"
exec 9>"$state/staging.lock"
flock -n 9

case "${1:-}" in
start)
    if mountpoint -q "$staging"; then
        [ "$(findmnt -rn -o SOURCE --target "$staging")" = "/dev/mapper/$mapping" ]
        cryptsetup status "$mapping" >/dev/null
        exit 0
    fi
    # An interrupted prepare needs inspection, never an implicit reformat.
    if cryptsetup status "$mapping" >/dev/null 2>&1; then
        echo 'unmounted staging mapping exists; inspect before retrying' >&2
        exit 1
    fi
    if [ -e "$image" ]; then
        [ "$(cat "$marker")" = 'flopwire encrypted scratch v1' ]
        [ "$(stat -c %s "$image")" = "$bytes" ]
    else
        # Refuse before allocation: a failed fallocate can leave a partial
        # allocation on the filesystem the running server also uses.
        available=$(df -B1 --output=avail "$state" | tail -n 1 | tr -d '[:space:]')
        case "$available" in ''|*[!0-9]*) echo 'cannot determine free space' >&2; exit 1;; esac
        if [ "$available" -lt "$bytes" ] || [ "$((available - bytes))" -lt "$reserve" ]; then
            echo 'insufficient disk headroom for encrypted staging' >&2
            exit 1
        fi
        if [ -e "$marker" ]; then
            [ "$(cat "$marker")" = 'flopwire encrypted scratch v1' ]
            rm "$marker"
        fi
        if ! fallocate -l "$bytes" "$image"; then
            rm -f "$image"
            exit 1
        fi
        printf '%s\n' 'flopwire encrypted scratch v1' >"$marker"
    fi
    cryptsetup open --type plain --cipher aes-xts-plain64 --key-size 512 \
        --key-file /dev/urandom "$image" "$mapping"
    mkfs.ext4 -q -m 0 "/dev/mapper/$mapping"
    mount -o nodev,nosuid,noexec "/dev/mapper/$mapping" "$staging"
    chmod 700 "$staging"
    ;;
stop)
    if mountpoint -q "$staging"; then
        [ "$(findmnt -rn -o SOURCE --target "$staging")" = "/dev/mapper/$mapping" ]
        umount "$staging"
    fi
    if cryptsetup status "$mapping" >/dev/null 2>&1; then
        cryptsetup close "$mapping"
    fi
    ;;
*) echo 'usage: prepare-backup-staging.sh start|stop' >&2; exit 2;;
esac
