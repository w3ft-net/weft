#!/bin/bash
# Demo service entrypoint.
#
# Reads from env:
#   SERVICE        — bare app name (api, worker, edge, orders, billing, notifier)
#   CUSTOMER       — customer name (acme, globex)
#   ENV            — sand | staging | prod
#   WEFTD_TOKEN    — wft_<...> raw token (per-service, env-bound)
#   RATE_SHAPE     — generator shape spec (see generator.py)
#   ERROR_RATIO    — float, fraction of lines emitted at ERROR level
#
# Renders /etc/weftd/config.yaml, kicks off the generator into
# /var/log/${SERVICE}.log, and execs weftd. weftd is PID 1 so SIGTERM
# from `docker stop` gets a clean shutdown.

set -euo pipefail

: "${SERVICE:?SERVICE required}"
: "${CUSTOMER:?CUSTOMER required}"
: "${ENV:?ENV required}"
: "${WEFTD_TOKEN:?WEFTD_TOKEN required}"
: "${RATE_SHAPE:=const:1}"
: "${ERROR_RATIO:=0.05}"

LOG_PATH="/var/log/${SERVICE}.log"
mkdir -p /var/log /etc/weftd

cat > /etc/weftd/config.yaml <<EOF
endpoint: "coordinator:9020"
tls: false # the dev coordinator serves plaintext
token: "${WEFTD_TOKEN}"
customer: "${CUSTOMER}"
deployment: "${SERVICE}"
sources:
  - app: ${SERVICE}
    path: ${LOG_PATH}
    metrics:
      enabled: true
      interval: 10s
EOF

# Make sure the file exists before weftd opens it; otherwise the agent
# may hit "no such file" on startup before the generator's first write.
touch "${LOG_PATH}"

# Background the generator. Trap SIGTERM/SIGINT and forward so docker
# stop tears the whole container down cleanly.
LOG_PATH="${LOG_PATH}" SERVICE="${SERVICE}" \
  RATE_SHAPE="${RATE_SHAPE}" ERROR_RATIO="${ERROR_RATIO}" \
  python3 /usr/local/bin/generator.py &
GEN_PID=$!

cleanup() {
  if kill -0 "${GEN_PID}" 2>/dev/null; then
    kill -TERM "${GEN_PID}" 2>/dev/null || true
  fi
}
trap cleanup TERM INT EXIT

exec /usr/local/bin/weftd --config /etc/weftd/config.yaml
