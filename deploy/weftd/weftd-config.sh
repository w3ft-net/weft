#!/bin/bash
# Renders /etc/weftd/config.yaml from the weftd environment.
# Called as ExecStartPre by weftd.service. Environment is loaded by systemd
# from /etc/default/weftd (see weftd.service's EnvironmentFile= directive).
#
# Supported variables (all optional except WEFT_TOKEN):
#   WEFT_TOKEN             agent auth token (required)
#   WEFT_ENDPOINT          coordinator ingest endpoint
#                          (default: ingest.w3ft.net:9020)
#   WEFT_APP               app name used for log tagging
#                          (default: $APP_NAME if set, else "unknown")
#   WEFT_UNIT              systemd unit to tail
#                          (default: ${WEFT_APP}.service)
#   WEFT_LOG_PATH          file path or glob to tail
#                          (default: /var/log/weftd/*.log)
#   WEFT_HOST_GROUP        logical group stamped on every row
#                          (e.g. "web", "worker", "edge-us-east")
#   WEFT_METRICS           "true" to enable system metrics (default: false)
#   WEFT_METRICS_INTERVAL  metric collection interval (default: 30s)
#   WEFT_SOCKET            unix socket for local line ingest
#                          (default: /var/run/weftd/weftd.sock)

set -euo pipefail

if [ -z "${WEFT_TOKEN:-}" ]; then
  echo "WEFT_TOKEN not set — edit /etc/default/weftd; skipping weftd config"
  exit 0
fi

APP=${WEFT_APP:-${APP_NAME:-unknown}}
UNIT=${WEFT_UNIT:-${APP}.service}
LOG_PATH=${WEFT_LOG_PATH:-/var/log/weftd/*.log}
SOCKET=${WEFT_SOCKET:-/var/run/weftd/weftd.sock}
METRICS_ENABLED=${WEFT_METRICS:-false}
METRICS_INTERVAL=${WEFT_METRICS_INTERVAL:-30s}

mkdir -p /etc/weftd

HOST_GROUP_LINE=""
if [ -n "${WEFT_HOST_GROUP:-}" ]; then
  HOST_GROUP_LINE="host_group: ${WEFT_HOST_GROUP}"
fi

cat > /etc/weftd/config.yaml <<EOF
endpoint: ${WEFT_ENDPOINT:-ingest.w3ft.net:9020}
token: ${WEFT_TOKEN}
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
echo "weftd config written: app=${APP} unit=${UNIT} host_group=${WEFT_HOST_GROUP:-<unset>}"
