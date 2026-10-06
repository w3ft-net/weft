#!/bin/bash
# Instance side of the ephemeral archive export
# (docs/plans/ephemeral-archive-export.md). Sourced by startup.sh when the
# instance's Role tag is "export"; defines functions only.
#
# An export host boots with a volume restored from a snapshot of a shard's
# volume attached at /dev/sdf. It mounts that volume at /mnt/export, runs
# `weft-shard export`, and shuts down. Shutdown is guaranteed on every path:
# the instance launches with InstanceInitiatedShutdownBehavior=terminate, so
# `shutdown` terminates it and DeleteOnTermination removes the restored volume.
#
# Deliberately NOT mounted at /opt/app/data: weftd reads shard.json there for
# its `node` dimension, and the restored volume carries the live shard's, so
# the export host's metrics would be attributed to the shard.

EXPORT_MOUNT=/mnt/export
EXPORT_ENV_FILE=/etc/weft-export.env
EXPORT_UNIT=weft-export
# Hard backstop: the host powers itself off this long after boot even if
# everything else (the unit, the wait loop) hangs. The binary's own --timeout
# and the coordinator's deadline are expected to fire first.
EXPORT_MAX_MINUTES="${EXPORT_MAX_MINUTES:-180}"

# export_env_contents SHARD_ID ATTEMPT PREFIX — the EnvironmentFile that
# weft-export.service reads. The values come from instance tags, so they are
# validated hard: a shard id is [A-Za-z0-9._-], the attempt a positive integer,
# and the prefix has no whitespace, quotes or shell/systemd metacharacters.
export_env_contents() {
    local shard="$1" attempt="$2" prefix="$3"
    case "$shard" in
        ''|*[!A-Za-z0-9._-]*) echo "export: bad shard id: '$shard'" >&2; return 1 ;;
    esac
    case "$attempt" in
        ''|0|0[0-9]*|*[!0-9]*) echo "export: bad attempt: '$attempt'" >&2; return 1 ;;
    esac
    case "$prefix" in
        ''|*[!A-Za-z0-9._:/-]*) echo "export: bad prefix: '$prefix'" >&2; return 1 ;;
    esac
    printf 'EXPORT_SHARD_ID=%s\nEXPORT_ATTEMPT=%s\nEXPORT_PREFIX=%s\n' "$shard" "$attempt" "$prefix"
}

# export_unit_running_state STATE — success while the unit is still going.
# `systemctl is-active` reports a running oneshot as "activating".
export_unit_running_state() {
    case "$1" in
        activating|active) return 0 ;;
        *) return 1 ;;
    esac
}

# export_imds_tag KEY — an instance tag via IMDSv2 (the export launch template
# enables instance metadata tags, as the shard one does).
export_imds_tag() {
    local token
    token=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60") || return 1
    curl -sf -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/tags/instance/$1"
}

export_shutdown() {
    echo "export: shutting down"
    shutdown -h now
}

# export_prepare — arm the shutdown guarantees, read the job from the instance
# tags, and mount the restored volume. Any failure exits non-zero; the EXIT trap
# then powers the host off.
export_prepare() {
    trap export_shutdown EXIT
    shutdown -h "+$EXPORT_MAX_MINUTES" 2>/dev/null || true

    local shard attempt prefix
    shard=$(export_imds_tag export_shard_id) || { echo "export: no export_shard_id tag" >&2; return 1; }
    attempt=$(export_imds_tag export_attempt) || { echo "export: no export_attempt tag" >&2; return 1; }
    prefix=$(export_imds_tag export_prefix) || { echo "export: no export_prefix tag" >&2; return 1; }
    export_env_contents "$shard" "$attempt" "$prefix" > "$EXPORT_ENV_FILE" || return 1
    chmod 644 "$EXPORT_ENV_FILE"

    # Find the restored volume by the device name it was attached as.
    local device="" nvme requested i
    for i in $(seq 1 30); do
        for nvme in /dev/nvme[1-9]n[0-9]*; do
            [ -e "$nvme" ] || continue
            requested=$(ebsnvme-id "$nvme" 2>/dev/null | awk '/Disk/{print $NF}')
            if [ "$requested" = "/dev/sdf" ] || [ "$requested" = "sdf" ]; then
                device=$nvme
                break 2
            fi
        done
        if [ -e /dev/sdf ]; then
            device=/dev/sdf
            break
        fi
        sleep 2
    done
    if [ -z "$device" ]; then
        echo "export: restored volume never appeared at /dev/sdf" >&2
        return 1
    fi
    # Never format. A shard's startup mkfs's a blank volume; here a blank volume
    # means a bad restore, and formatting it would turn that into an empty
    # "successful" archive.
    if ! blkid "$device" >/dev/null 2>&1; then
        echo "export: $device has no filesystem; refusing to format a restored volume" >&2
        return 1
    fi
    mkdir -p "$EXPORT_MOUNT"
    mount -o defaults,noatime "$device" "$EXPORT_MOUNT" || return 1
    chown -R app:app "$EXPORT_MOUNT"
    echo "export: restored volume $device mounted at $EXPORT_MOUNT"
}

# export_finish — block until the export unit finishes, give weftd time to ship
# its last log lines, then shut down.
export_finish() {
    local state
    while true; do
        state=$(systemctl is-active "$EXPORT_UNIT" 2>/dev/null || true)
        export_unit_running_state "$state" || break
        sleep 5
    done
    echo "export: unit finished: $(systemctl show -p Result --value "$EXPORT_UNIT" 2>/dev/null)"
    sleep 30
}
