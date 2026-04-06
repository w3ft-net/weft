#!/bin/bash
# Generates /etc/weftd/config.yaml from environment variables.
# Called as ExecStartPre by weftd.service.
#
# Expects:
#   WEFT_TOKEN    - agent auth token (from Secrets Manager via fetch-config.sh)
#   APP_NAME      - app name for log tagging (from SSM config or kevcons)
#   WEFT_ENDPOINT - coordinator ingest endpoint (default: ingest.w3ft.net:9020)
#   WEFT_UNIT     - systemd unit to tail (default: ${APP_NAME}.service)

set -euo pipefail

# Source the app's env file if it exists (written by fetch-config.sh)
[ -f /opt/app/.env ] && source /opt/app/.env

if [ -z "${WEFT_TOKEN:-}" ]; then
  echo "WEFT_TOKEN not set — skipping weftd config"
  exit 0
fi

APP=${APP_NAME:-unknown}
UNIT=${WEFT_UNIT:-${APP}.service}
LOG_PATH=${WEFT_LOG_PATH:-/opt/app/logs/*.log}

mkdir -p /etc/weftd

cat > /etc/weftd/config.yaml <<EOF
endpoint: ${WEFT_ENDPOINT:-ingest.w3ft.net:9020}
token: ${WEFT_TOKEN}
sources:
  - app: ${APP}
    unit: ${UNIT}
  - app: ${APP}
    path: ${LOG_PATH}
socket: /var/run/weftd/weftd.sock
EOF

chmod 600 /etc/weftd/config.yaml
echo "weftd config written for app=${APP} unit=${UNIT}"
