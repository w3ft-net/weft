#!/bin/bash
# weft startup — runs as weft-startup.service oneshot on boot. Reads
# the instance's Role tag, refreshes /opt/app/.env via fetch-config.sh
# (which delegates to load-app-env), translates the populated env into
# role-specific EnvironmentFile(s) for systemd, mounts the shard data
# volume if needed, and starts services.
#
# Source of truth for coordinator/api flags is the kevcons-managed
# config blob at /weft/config/app, surfaced through load-app-env. This
# script just threads those values into the systemd EnvironmentFile;
# it does not fetch anything from SSM directly anymore (aside from the
# standalone db-dsn param, which fetch-config.sh handles).

set -euo pipefail

# Get instance metadata
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Read the Role tag
ROLE=$(aws ec2 describe-tags \
  --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=Role" \
  --region "$REGION" \
  --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

if [ -z "$ROLE" ] || [ "$ROLE" = "None" ]; then
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

# Coordinator: write /opt/app/coordinator.env with a sanitised,
# plain-KEY=VALUE form that systemd's EnvironmentFile parser
# accepts. All values use the blob keys directly (no WEFT_ prefix)
# — the Go side reads DB_DSN, S3_REGION, etc. as-is, no translation
# layer needed.
if [ "$ROLE" = "coordinator" ]; then
  # Safety belt: the whole point of this script post-rewrite is to
  # avoid the silent fall-through to dev mode we saw on 2026-04-20.
  # If the blob didn't populate ASG names, bail loudly — an operator
  # can fix the config much faster than they can notice a dev-mode
  # coordinator running in prod.
  if [ -z "${SHARD_ASG_SMALL:-}" ]; then
    echo "startup: FATAL — SHARD_ASG_SMALL missing from env after fetch-config"
    echo "startup: check 'kevcons config list' or /weft/config/app SSM blob"
    exit 1
  fi

  cat > /opt/app/coordinator.env <<EOF
APP_NAME=${APP_NAME}
AWS_REGION=${REGION}
DB_DSN=${DB_DSN:-}
S3_REGION=${REGION}
SHARD_ASG_SMALL=${SHARD_ASG_SMALL:-}
SHARD_ASG_MEDIUM=${SHARD_ASG_MEDIUM:-}
SHARD_ASG_LARGE=${SHARD_ASG_LARGE:-}
COMPUTE_ASG_NAME=${COMPUTE_ASG_NAME:-}
PARQUET_DIR=${PARQUET_DIR:-}
LAMBDA_FUNCTION_URL=${LAMBDA_FUNCTION_URL:-}
LIFECYCLE_QUEUE_URL=${LIFECYCLE_QUEUE_URL:-}
SHARD_VOLUME_DEVICE=${SHARD_VOLUME_DEVICE:-}
SHARD_SMALL_VOLUME_GB=${SHARD_SMALL_VOLUME_GB:-}
SHARD_MEDIUM_VOLUME_GB=${SHARD_MEDIUM_VOLUME_GB:-}
SHARD_LARGE_VOLUME_GB=${SHARD_LARGE_VOLUME_GB:-}
JWT_SECRET=${JWT_SECRET:-}
SHARD_SECRET=${SHARD_SECRET:-}
STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY:-}
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET:-}
STRIPE_PRICE_SWATCH=price_1TK7iK2SENTbrh5us0SI1nRz
STRIPE_PRICE_YARD=price_1TK7jI2SENTbrh5uP3wVxnlx
STRIPE_PRICE_BOLT=price_1TK7kH2SENTbrh5upDvBswnG
EMAIL_HOST_PASSWORD=${EMAIL_HOST_PASSWORD:-}
SLACK_CLIENT_ID=${SLACK_CLIENT_ID:-}
SLACK_CLIENT_SECRET=${SLACK_CLIENT_SECRET:-}
EOF
  chown app:app /opt/app/coordinator.env
fi

# For shard roles: generate shard.env and mount the per-shard EBS data
# volume at /opt/app/data before the service starts.
# Shard identity lives in /opt/app/data/shard.json on the EBS volume —
# the coordinator writes it via the Configure RPC on first christening
# and every subsequent restart reads it back. See
# docs/plans/ebs-shard-volumes.md.
if [ "$ROLE" = "shard" ]; then
  cat > /opt/app/shard.env <<EOF
COORDINATOR_ADDR=${COORDINATOR_NLB:-localhost:9020}
SHARD_SECRET=${SHARD_SECRET:-}
EOF
  chown app:app /opt/app/shard.env
  echo "shard env: coordinator=${COORDINATOR_NLB:-localhost:9020}"

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

systemctl start "weft-$ROLE"

# Also start the API server on coordinator hosts.
if [ "$ROLE" = "coordinator" ]; then
  systemctl start weft-api
fi

# Start weftd on all roles if a token is available. WEFTD_TOKEN is
# populated by load-app-env from the /weft/WEFTD_TOKEN secret.
if [ -n "${WEFTD_TOKEN:-}" ] && command -v weftd >/dev/null 2>&1; then
  mkdir -p /etc/weftd
  cat > /etc/weftd/config.yaml <<WEFTCFG
endpoint: ingest.w3ft.net:9020
token: ${WEFTD_TOKEN}
host_group: ${ROLE}
sources:
  - path: /opt/app/logs/*.log
    metrics:
      enabled: true
      interval: 30s
      collectors:
        - cpu
        - memory
        - disk
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
