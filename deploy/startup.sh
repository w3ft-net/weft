#!/bin/bash
# weft startup — runs as weft-startup.service oneshot on boot. Reads
# the instance's Role tag, refreshes /opt/app/.env via fetch-config.sh
# (which delegates to load-app-env), mounts the shard data volume if
# needed, and starts services.
#
# Source of truth for coordinator/api/shard flags is the kevcons-
# managed config blob at /weft/config/app, surfaced through
# load-app-env into /opt/app/.env. The systemd service files read
# that file directly as their EnvironmentFile — no translation layer,
# no whitelist.

set -euo pipefail

# Get instance metadata
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Read the Role tag via IMDS. Relies on instance_metadata_tags=enabled
# on the launch template (modules/asg-application). No IAM round-trip
# and no silent-failure mode from an API error.
ROLE=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/tags/instance/Role || true)

if [ -z "$ROLE" ]; then
  echo "No Role tag found on instance $INSTANCE_ID, defaulting to coordinator"
  ROLE="coordinator"
fi

echo "Starting weft-$ROLE on instance $INSTANCE_ID in $REGION"
APP_NAME="weft"

# Refresh /opt/app/.env from the blob + secrets + standalone db-dsn.
# user-data has already done this once at launch, but re-running here
# makes startup.sh self-contained and idempotent: reload-config.sh can
# invoke it directly, and a manual restart picks up any post-boot
# config changes that landed before the service came back.
APP_NAME="$APP_NAME" AWS_REGION="$REGION" /opt/app/scripts/fetch-config.sh "$APP_NAME"

set -a
. /opt/app/.env
set +a

# Export role: an ephemeral archive-export host (see deploy/export-role.sh and
# docs/plans/ephemeral-archive-export.md). It mounts a restored copy of a
# shard's volume at /mnt/export — not /opt/app/data, so it can't pass for the
# shard — and powers itself off when done. export_prepare arms the shutdown
# guarantee first thing; if the library itself is missing, shut down now.
if [ "$ROLE" = "export" ]; then
  . /opt/app/scripts/export-role.sh || shutdown -h now
  export_prepare
fi

# Safety belt for coordinator: if the blob didn't populate the shard
# launch config, bail loudly. An operator can fix the config much
# faster than they can notice a dev-mode coordinator running in prod —
# a regression we actually hit on 2026-04-20. Shard + compute have no
# such sanity invariant at startup.
#
# This used to key off SHARD_ASG_SMALL, but the 2026-06-05 shard-ASG
# drop removed that var (shards now launch via ec2:RunInstances against
# SHARD_SMALL_LAUNCH_TEMPLATE). The stale guard then aborted every
# coordinator launch and took prod down on 2026-06-06; repointed at the
# launch-template key, which is the current "blob populated" signal.
if [ "$ROLE" = "coordinator" ]; then
  if [ -z "${SHARD_SMALL_LAUNCH_TEMPLATE:-}" ]; then
    echo "startup: FATAL — SHARD_SMALL_LAUNCH_TEMPLATE missing from env after fetch-config"
    echo "startup: check 'kevcons config list' or /weft/config/app SSM blob"
    exit 1
  fi
fi

# For shard roles: mount the per-shard EBS data volume at
# /opt/app/data before the service starts. Shard identity lives in
# /opt/app/data/shard.json on the EBS volume — the coordinator writes
# it via the Configure RPC on first christening and every subsequent
# restart reads it back. See docs/plans/ebs-shard-volumes.md.
if [ "$ROLE" = "shard" ]; then
  echo "shard role: coordinator=${COORDINATOR_NLB:-localhost:9020}"

  # Mount the data volume. The coordinator attaches an EBS volume at
  # /dev/sdf before completing the instance's ASG lifecycle hook; on
  # nitro instances the kernel renames it to /dev/nvme1n1 or similar.
  # Resolve it via ec2-utils (available in all our shard AMIs). If no
  # data volume is attached within ~60s, fall through — the shard can
  # still come up on the root volume for dev or container mode.
  MOUNT_POINT=/opt/app/data
  DEVICE=""
  for i in $(seq 1 30); do
    # ebsnvme-id is shipped with ec2-utils on Amazon Linux and Debian
    # AMIs we build from. It maps /dev/nvme*n* → the requested device
    # name we asked for at attach time.
    for nvme in /dev/nvme[1-9]n[0-9]*; do
      [ -e "$nvme" ] || continue
      requested=$(ebsnvme-id "$nvme" 2>/dev/null | awk '/Disk/{print $NF}')
      if [ "$requested" = "/dev/sdf" ] || [ "$requested" = "sdf" ]; then
        DEVICE=$nvme
        break 2
      fi
    done
    # On non-nitro / older kernels, /dev/sdf may be the literal path.
    if [ -e "/dev/sdf" ]; then
      DEVICE=/dev/sdf
      break
    fi
    sleep 2
  done

  if [ -n "$DEVICE" ]; then
    echo "shard data volume detected at $DEVICE"
    if ! blkid "$DEVICE" >/dev/null 2>&1; then
      echo "formatting $DEVICE (fresh volume)"
      mkfs.ext4 -L weft-data "$DEVICE"
    fi
    mkdir -p "$MOUNT_POINT"
    # Use LABEL= so a kernel-induced device rename across reboots
    # doesn't break the mount.
    if ! mountpoint -q "$MOUNT_POINT"; then
      mount -o defaults,noatime LABEL=weft-data "$MOUNT_POINT" \
        || mount -o defaults,noatime "$DEVICE" "$MOUNT_POINT"
    fi
    chown -R app:app "$MOUNT_POINT"
    echo "shard data volume mounted at $MOUNT_POINT"
  else
    echo "no shard data volume attached; running on root volume (dev/container mode)"
    mkdir -p "$MOUNT_POINT"
    chown -R app:app "$MOUNT_POINT"
  fi
fi

# The export unit is a long oneshot: don't block here on it, so weftd below is
# up (and shipping its log) while the export runs. export_finish waits for it.
if [ "$ROLE" = "export" ]; then
  systemctl start --no-block "weft-$ROLE"
else
  systemctl start "weft-$ROLE"
fi

# Also start the API server on coordinator hosts.
if [ "$ROLE" = "coordinator" ]; then
  systemctl start weft-api
fi

# Start weftd on all roles if a token is available. WEFTD_TOKEN is
# populated by load-app-env from the /weft/WEFTD_TOKEN secret.
#
# Skip on the loom-producer role: that host runs its own weftd-loom@
# instances under per-env LOOM_*_TOKEN values, and the ops-side
# WEFTD_TOKEN would conflict with the host's customer/env binding.
if [ "$ROLE" != "loom-producer" ] && [ -n "${WEFTD_TOKEN:-}" ] && command -v weftd >/dev/null 2>&1; then
  mkdir -p /etc/weftd
  # The coordinator role co-hosts api today; both surfaces want their
  # own /log/weft/<service>/{metrics,heartbeats}.log streams. Other
  # roles ship metrics under their single role name.
  case "$ROLE" in
    coordinator) SERVICES='[coordinator, api]' ;;
    *)           SERVICES="[${ROLE}]" ;;
  esac

  # The coordinator writes its query-audit stream to a dedicated file
  # (--query-log in weft-coordinator.service). Tail it as its own
  # (app=coordinator, kind=queries) source so it lands at
  # /log/weft/coordinator/queries.log instead of folding into
  # coordinator.log — substring monitors watching weft/coordinator must
  # not match the audit probe that produced the line. weftd's
  # glob/explicit deconfliction keeps the /opt/app/logs/*.log source
  # below from also ingesting this file. Coordinator role only.
  QUERIES_SOURCE=""
  if [ "$ROLE" = "coordinator" ]; then
    QUERIES_SOURCE="  - path: /opt/app/logs/coordinator-queries.log
    service: coordinator
    kind: queries"
  fi

  cat > /etc/weftd/config.yaml <<WEFTCFG
token: ${WEFTD_TOKEN}
deployment: ${ROLE}
socket: /var/run/weftd.sock
services: ${SERVICES}
dimensions:
  # \$NODE = stable workload identity. Reads shard.json's shard_id
  # on shard hosts; non-shard hosts have no shard.json so the column
  # stays NULL there. Live-watched: when the provisioner christens a
  # blank shard mid-run by writing shard.json, subsequent metrics rows
  # pick up the new ID without restarting weftd.
  node: /opt/app/data/shard.json:.shard_id
sources:
  - path: /opt/app/logs/*.log
    metrics:
      enabled: true
      interval: 30s
      collectors:
        - cpu
        - memory
        - disk
${QUERIES_SOURCE}
WEFTCFG
  chmod 600 /etc/weftd/config.yaml

  # Create systemd unit if not already present.
  if [ ! -f /etc/systemd/system/weftd.service ]; then
    cat > /etc/systemd/system/weftd.service <<'UNIT'
[Unit]
Description=weftd log agent
After=network.target

[Service]
Type=simple
StateDirectory=weftd
ExecStart=/usr/local/bin/weftd --config=/etc/weftd/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
  fi

  systemctl enable weftd
  systemctl restart weftd
  echo "weftd started for role=$ROLE"
fi

# Export role: hold the host until the export unit finishes (and weftd has
# shipped its last lines), then return — the EXIT trap armed in export_prepare
# powers the host off.
if [ "$ROLE" = "export" ]; then
  export_finish
fi
