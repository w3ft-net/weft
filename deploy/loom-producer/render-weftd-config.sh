#!/bin/bash
# render-weftd-config.sh ENV — render /etc/weftd-loom-${ENV}/config.yaml
# for the producer host's weftd-loom@${ENV}.service unit.
#
# Called from the unit's ExecStartPre. /opt/app/.env (loaded by systemd
# via the unit's EnvironmentFile=) carries every /weft/* secret that
# load-app-env discovered, including LOOM_PROD_TOKEN and
# LOOM_SAND_TOKEN. We pick the one matching $ENV and emit a weftd
# config with all three Loom apps for that env as separate sources, so
# a single weftd ships api+worker+edge under one token.
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

# Bash indirect expansion: VAR holds the literal name "LOOM_PROD_TOKEN"
# or "LOOM_SAND_TOKEN", and ${!VAR} dereferences to the env value.
VAR="LOOM_${ENV^^}_TOKEN"
TOKEN="${!VAR:-}"

if [ -z "$TOKEN" ]; then
  echo "render-weftd-config.sh: ${VAR} not set in /opt/app/.env" >&2
  echo "  populate it via:  kevcons secrets set ${VAR} <token>" >&2
  echo "  then:             systemctl restart weft-startup weftd-loom@${ENV}" >&2
  exit 1
fi

OUT_DIR="/etc/weftd-loom-${ENV}"
mkdir -p "$OUT_DIR"

cat > "${OUT_DIR}/config.yaml" <<EOF
endpoint: ingest.w3ft.net:9020
token: ${TOKEN}
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
