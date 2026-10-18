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
# (tr rather than ${ENV^^} so this runs under bash 3.2 too — macOS dev hosts.)
ENV_UC=$(printf '%s' "$ENV" | tr '[:lower:]' '[:upper:]')
VAR="LOOM_${ENV_UC}_TOKEN"
TOKEN="${!VAR:-}"

if [ -z "$TOKEN" ]; then
  echo "render-weftd-config.sh: ${VAR} not set in /opt/app/.env" >&2
  echo "  populate it via:  kevcons secrets set ${VAR} <token>" >&2
  echo "  then:             systemctl restart weft-startup weftd-loom@${ENV}" >&2
  exit 1
fi

# OUT_DIR is overridable so tests can render into a temp dir; production
# leaves it unset and writes to the fixed /etc path the units read.
OUT_DIR="${WEFTD_LOOM_CONFDIR:-/etc/weftd-loom-${ENV}}"
mkdir -p "$OUT_DIR"

# 0.20.0 config schema: sources use `service:` (was `app:`) and the
# fleet/scaling-unit field is `deployment:` (was `host_group:`). An
# unknown key is silently ignored by weftd's lenient YAML, so a stale
# `app:` would leave the service blank and weftd would fall back to
# basename(path) → "api-prod" instead of "api".
cat > "${OUT_DIR}/config.yaml" <<EOF
token: ${TOKEN}
deployment: loom-producer
metrics:
  interval: 30s
  collectors:
    - memory
sources:
  - service: api
    path: /var/log/loom/api-${ENV}.log
  - service: worker
    path: /var/log/loom/worker-${ENV}.log
  - service: edge
    path: /var/log/loom/edge-${ENV}.log
EOF
chmod 600 "${OUT_DIR}/config.yaml"

# token.env carries the loom token as WEFTD_TOKEN. The weftd-loom@ unit
# loads this *after* /opt/app/.env, so it overrides the fleet WEFTD_TOKEN
# that load-app-env drops on every host. weftd's token chain prefers the
# env value over config.yaml's token: (main.go tokenFromChain), so
# without this the forwarder would authenticate as the ops fleet and
# loom's logs would land in graybeard/weft instead of loom/. The
# config.yaml token: stays as a matching belt-and-suspenders fallback.
cat > "${OUT_DIR}/token.env" <<EOF
WEFTD_TOKEN=${TOKEN}
EOF
chmod 600 "${OUT_DIR}/token.env"

echo "render-weftd-config: wrote ${OUT_DIR}/config.yaml and ${OUT_DIR}/token.env"
