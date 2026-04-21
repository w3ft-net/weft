#!/bin/bash
# Renders /etc/weftd/config.yaml from the weftd environment.
# Called as ExecStartPre by weftd.service. Environment is loaded by systemd
# from /etc/default/weftd (see weftd.service's EnvironmentFile= directive).
#
# All weftd-specific variables use the WEFTD_ prefix to avoid colliding
# with the coordinator's env (DB_DSN, S3_REGION, SHARD_ASG_*, etc.) on
# hosts that run both. See /etc/default/weftd for the full list.
#
# Supported variables (all optional except WEFTD_TOKEN):
#   WEFTD_TOKEN             agent auth token (required)
#   WEFTD_ENDPOINT          coordinator ingest endpoint
#                           (default: ingest.w3ft.net:9020)
#   WEFTD_APP               app name used for log tagging
#                           (default: $APP_NAME if set, else "unknown")
#   WEFTD_UNIT              systemd unit to tail
#                           (default: ${WEFTD_APP}.service)
#   WEFTD_LOG_PATH          file path or glob to tail
#                           (default: /var/log/weftd/*.log)
#   WEFTD_HOST_GROUP        logical group stamped on every row
#                           (e.g. "web", "worker", "edge-us-east")
#   WEFTD_METRICS           "true" to enable system metrics (default: false)
#   WEFTD_METRICS_INTERVAL  metric collection interval (default: 30s)
#   WEFTD_SOCKET            unix socket for local line ingest
#                           (default: /var/run/weftd/weftd.sock)

set -euo pipefail

if [ -z "${WEFTD_TOKEN:-}" ]; then
  echo "WEFTD_TOKEN not set — edit /etc/default/weftd; skipping weftd config"
  exit 0
fi

APP=${WEFTD_APP:-${APP_NAME:-unknown}}
UNIT=${WEFTD_UNIT:-${APP}.service}
LOG_PATH=${WEFTD_LOG_PATH:-/var/log/weftd/*.log}
SOCKET=${WEFTD_SOCKET:-/var/run/weftd/weftd.sock}
METRICS_ENABLED=${WEFTD_METRICS:-false}
METRICS_INTERVAL=${WEFTD_METRICS_INTERVAL:-30s}

mkdir -p /etc/weftd

HOST_GROUP_LINE=""
if [ -n "${WEFTD_HOST_GROUP:-}" ]; then
  HOST_GROUP_LINE="host_group: ${WEFTD_HOST_GROUP}"
fi

cat > /etc/weftd/config.yaml <<EOF
endpoint: ${WEFTD_ENDPOINT:-ingest.w3ft.net:9020}
token: ${WEFTD_TOKEN}
${HOST_GROUP_LINE}
sources:
  - unit: ${UNIT}
    metrics:
      enabled: ${METRICS_ENABLED}
      interval: ${METRICS_INTERVAL}
  - path: ${LOG_PATH}
socket: ${SOCKET}
EOF

chmod 600 /etc/weftd/config.yaml
echo "weftd config written: app=${APP} unit=${UNIT} host_group=${WEFTD_HOST_GROUP:-<unset>}"
