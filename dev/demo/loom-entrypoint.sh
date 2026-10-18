#!/bin/bash
# Dev Loom producer entrypoint — the prod producer host's topology in one
# container: loom-producer writes /var/log/loom/<app>-<env>.log for all six
# streams, and one weftd per env forwards that env's three apps under the
# env-bound agent token (what deploy/loom-producer/render-weftd-config.sh and
# the weftd-loom@ units do on the host).
#
# Reads from env:
#   LOOM_PROD_TOKEN, LOOM_SAND_TOKEN — raw agent tokens, minted by weft-demo-seed
#
# The last weftd is exec'd so it is PID 1 and `docker stop` gets a clean
# shutdown; the producer and the other forwarder are torn down with it.

set -euo pipefail

: "${LOOM_PROD_TOKEN:?LOOM_PROD_TOKEN required}"
: "${LOOM_SAND_TOKEN:?LOOM_SAND_TOKEN required}"

mkdir -p /var/log/loom

render_weftd_config() {
  local env="$1" token="$2"
  mkdir -p "/etc/weftd-loom-${env}"
  # 0.20.0 config schema: `service:` per source, `deployment:` for the fleet.
  cat > "/etc/weftd-loom-${env}/config.yaml" <<YAML
endpoint: "coordinator:9020"
tls: false # the dev coordinator serves plaintext
token: ${token}
deployment: loom-producer
metrics:
  interval: 10s
sources:
  - service: api
    path: /var/log/loom/api-${env}.log
  - service: worker
    path: /var/log/loom/worker-${env}.log
  - service: edge
    path: /var/log/loom/edge-${env}.log
YAML
  # Files must exist before weftd opens them.
  touch "/var/log/loom/api-${env}.log" "/var/log/loom/worker-${env}.log" "/var/log/loom/edge-${env}.log"
}

render_weftd_config prod "${LOOM_PROD_TOKEN}"
render_weftd_config sand "${LOOM_SAND_TOKEN}"

pids=()
cleanup() {
  for pid in "${pids[@]}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
}
trap cleanup TERM INT EXIT

loom-producer --config /etc/loom-producer/streams.yaml &
pids+=($!)

# weftd prefers WEFTD_TOKEN from the environment over config.yaml's token.
WEFTD_TOKEN="${LOOM_SAND_TOKEN}" weftd --config /etc/weftd-loom-sand/config.yaml &
pids+=($!)

WEFTD_TOKEN="${LOOM_PROD_TOKEN}" exec weftd --config /etc/weftd-loom-prod/config.yaml
