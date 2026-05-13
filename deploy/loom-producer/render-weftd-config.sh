#!/bin/bash
# render-weftd-config.sh ENV — render /etc/weftd-loom-${ENV}/config.yaml
# for the producer host's weftd-loom@${ENV}.service unit.
#
# Called from the unit's ExecStartPre. Reads WEFTD_TOKEN from the
# unit's environment (sourced via EnvironmentFile from
# /etc/default/weftd-loom-${ENV}); the config it emits lists all three
# Loom apps for the given env as separate sources, each pointing at
# /var/log/loom/${app}-${ENV}.log, so a single weftd ships all three
# apps under one token.
#
# Bare-bones on purpose — the source list is hardcoded here because
# the Loom app set (api / worker / edge) is also hardcoded in
# /etc/loom-producer/streams.yaml. If someone adds a fourth app, both
# files have to grow.

set -euo pipefail

ENV="${1:?usage: render-weftd-config.sh ENV}"
case "$ENV" in
  prod|sand) ;;
  *) echo "render-weftd-config.sh: unknown env: $ENV" >&2; exit 2 ;;
esac

: "${WEFTD_TOKEN:?WEFTD_TOKEN missing from /etc/default/weftd-loom-${ENV}}"

OUT_DIR="/etc/weftd-loom-${ENV}"
mkdir -p "$OUT_DIR"

cat > "${OUT_DIR}/config.yaml" <<EOF
endpoint: ingest.w3ft.net:9020
token: ${WEFTD_TOKEN}
host_group: loom-producer
sources:
  - app: api
    path: /var/log/loom/api-${ENV}.log
  - app: worker
    path: /var/log/loom/worker-${ENV}.log
  - app: edge
    path: /var/log/loom/edge-${ENV}.log
EOF

chmod 600 "${OUT_DIR}/config.yaml"
echo "render-weftd-config: wrote ${OUT_DIR}/config.yaml"
