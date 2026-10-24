#!/bin/bash
# shard-volume.sh — mount and unmount the shard's EBS data volume at
# /opt/app/data. Root-owned; the shard (running as app) calls exactly
#
#   sudo -n /usr/local/sbin/weft-shard-volume mount
#   sudo -n /usr/local/sbin/weft-shard-volume unmount
#
# through deploy/sudoers-weft-shard, and startup.sh (as root) calls
# `mount --within <seconds>` at boot. See docs/plans/shard-seamless-upgrade.md.
#
#   mount             wait (indefinitely) for the volume the coordinator
#                     attaches at /dev/sdf, format it if it is blank, mount
#                     it. A warm-pool spare's activation and a paused
#                     shard's Resume use this: the attach is on its way.
#   mount --within N  the same, but give up after N seconds with exit 3 (no
#                     volume attached). startup.sh's boot path, which then
#                     falls back to the root volume (dev/container mode).
#   unmount           sync and unmount, so the coordinator can detach the
#                     volume cleanly. A busy filesystem fails (non-zero):
#                     the coordinator must not detach it.
#
# Every verb is idempotent.

MOUNT_POINT="${SHARD_VOLUME_MOUNT_POINT:-/opt/app/data}"
DEV_ROOT="${SHARD_VOLUME_DEV_ROOT:-/dev}"
LABEL=weft-data

# resolve_device prints the block device the coordinator attached as
# /dev/sdf, or nothing. On nitro instances the kernel names it
# /dev/nvmeXn1; ebsnvme-id (ec2-utils, in every shard AMI) maps it back to
# the name requested at attach time. Older kernels keep /dev/sdf.
resolve_device() {
    local nvme requested
    for nvme in "$DEV_ROOT"/nvme[1-9]n[0-9]*; do
        [ -e "$nvme" ] || continue
        requested=$(ebsnvme-id "$nvme" 2>/dev/null | awk '/Disk/{print $NF}')
        if [ "$requested" = "/dev/sdf" ] || [ "$requested" = "sdf" ]; then
            echo "$nvme"
            return 0
        fi
    done
    if [ -e "$DEV_ROOT/sdf" ]; then
        echo "$DEV_ROOT/sdf"
    fi
}

# wait_for_device prints the device once it appears, checking every 100ms:
# on a spare this is inside an upgrade's hold window. within=0 waits
# forever; otherwise it gives up after roughly that many seconds.
wait_for_device() {
    local within="$1" polls=0 dev
    while :; do
        dev=$(resolve_device)
        if [ -n "$dev" ]; then
            echo "$dev"
            return 0
        fi
        if [ "$within" -gt 0 ] && [ "$polls" -ge $((within * 10)) ]; then
            return 1
        fi
        sleep 0.1
        polls=$((polls + 1))
    done
}

volume_mount() {
    local within="$1" dev
    if mountpoint -q "$MOUNT_POINT"; then
        return 0
    fi
    if ! dev=$(wait_for_device "$within"); then
        echo "shard-volume: no data volume attached within ${within}s" >&2
        return 3
    fi
    if ! blkid "$dev" >/dev/null 2>&1; then
        echo "shard-volume: formatting $dev (fresh volume)"
        mkfs.ext4 -L "$LABEL" "$dev"
    fi
    mkdir -p "$MOUNT_POINT"
    mount -o defaults,noatime "$dev" "$MOUNT_POINT"
    chown -R app:app "$MOUNT_POINT"
    echo "shard-volume: $dev mounted at $MOUNT_POINT"
}

volume_unmount() {
    if ! mountpoint -q "$MOUNT_POINT"; then
        return 0
    fi
    sync
    umount "$MOUNT_POINT"
    echo "shard-volume: unmounted $MOUNT_POINT"
}

main() {
    case "${1:-}" in
    mount)
        if [ "${2:-}" = "--within" ]; then
            [[ "${3:-}" =~ ^[0-9]+$ ]] || { echo "usage: $0 mount [--within SECONDS]" >&2; return 2; }
            volume_mount "$3"
        elif [ $# -eq 1 ]; then
            volume_mount 0
        else
            echo "usage: $0 mount [--within SECONDS]" >&2
            return 2
        fi
        ;;
    unmount)
        [ $# -eq 1 ] || { echo "usage: $0 unmount" >&2; return 2; }
        volume_unmount
        ;;
    *)
        echo "usage: $0 mount [--within SECONDS] | unmount" >&2
        return 2
        ;;
    esac
}

# Sourced by the tests; executed by sudo and startup.sh.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    set -euo pipefail
    main "$@"
fi
