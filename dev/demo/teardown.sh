#!/bin/bash
# Tear down the multi-customer demo:
#   1. stop and remove the 12 demo service containers
#   2. delete the acme + globex customers (cascades)
#   3. clear dev/demo/tokens.env
#
# Leaves the base stack (postgres, coordinator, kevlar dev customer) alone.
#
#   ./dev/demo/teardown.sh

set -euo pipefail

cd "$(dirname "$0")/../.."  # repo root

DSN="${DB_DSN:-host=localhost port=5433 user=weft password=weft dbname=weft sslmode=disable}"
TOKENS="dev/demo/tokens.env"
SEED_BIN="dev/demo/weft-demo-seed"

echo "→ stopping demo containers"
if [ -f "${TOKENS}" ]; then
  docker compose \
    -f docker-compose.yml \
    -f docker-compose.demo.yml \
    --env-file "${TOKENS}" \
    rm -sf \
    acme-api-sand acme-api-prod \
    acme-worker-sand acme-worker-prod \
    acme-edge-sand acme-edge-prod \
    globex-orders-staging globex-orders-prod \
    globex-billing-staging globex-billing-prod \
    globex-notifier-staging globex-notifier-prod || true
else
  echo "  (no tokens.env — skipping container rm)"
fi

if [ ! -x "${SEED_BIN}" ]; then
  echo "→ building seed CLI"
  go build -o "${SEED_BIN}" ./cmd/weft-demo-seed
fi

echo "→ deleting demo customers (cascade)"
"./${SEED_BIN}" --db-dsn "${DSN}" --reset

echo "→ removing tokens.env"
rm -f "${TOKENS}"

echo "→ done."
