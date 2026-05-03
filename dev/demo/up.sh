#!/bin/bash
# Bring up the multi-customer demo:
#   1. ensure base stack (postgres, coordinator, etc.) is running
#   2. mint tokens via weft-demo-seed (idempotent; skipped if tokens.env exists)
#   3. start the 12 demo service containers
#
# Re-mint tokens by deleting dev/demo/tokens.env first.
#
#   ./dev/demo/up.sh

set -euo pipefail

cd "$(dirname "$0")/../.."  # repo root
REPO_ROOT="$(pwd)"

DSN="${DB_DSN:-host=localhost port=5433 user=weft password=weft dbname=weft sslmode=disable}"
TOKENS="dev/demo/tokens.env"
SEED_BIN="dev/demo/weft-demo-seed"

echo "→ ensuring base stack is up"
docker compose up -d postgres coordinator api

echo "→ building seed CLI"
go build -o "${SEED_BIN}" ./cmd/weft-demo-seed

echo "→ seeding customers/users/tokens (idempotent)"
# A fresh dev DB is virgin (no goose_db_version table yet) so
# store.Migrate has to apply schema.sql via psql for bootstrap. That
# path is gated by WEFT_ALLOW_SCHEMA_BOOTSTRAP — production never
# hits it (the prod DB is always already bootstrapped), and we'd
# rather fail loud than silently restore a stale embedded snapshot
# if it ever did.
WEFT_ALLOW_SCHEMA_BOOTSTRAP=1 "./${SEED_BIN}" --db-dsn "${DSN}" --tokens-out "${TOKENS}"

echo "→ bringing up 12 demo services"
docker compose \
  -f docker-compose.yml \
  -f docker-compose.demo.yml \
  --env-file "${TOKENS}" \
  up -d --build \
  acme-api-sand acme-api-prod \
  acme-worker-sand acme-worker-prod \
  acme-edge-sand acme-edge-prod \
  globex-orders-staging globex-orders-prod \
  globex-billing-staging globex-billing-prod \
  globex-notifier-staging globex-notifier-prod

cat <<'EOF'
→ done.

Logging in (the auth surface is HTTP-only — no email is sent in dev):

  1. Start the frontend dev server (separate terminal):
       cd frontend && npx vite --port 5173

  2. Open http://localhost:5173 and submit one of the seeded emails.
     Acme team:    alice@acme.test, bob@acme.test, carol@acme.test
     Globex team:  dana@globex.test, eli@globex.test, frida@globex.test

  3. Grab the magic link from the api container's log
     (no real email is sent in dev):
       docker logs --tail 20 weft-api-1 | grep "magic link"

  4. Paste the URL into the browser to complete sign-in.

  CLI access uses an OAuth-style device-code flow:

       WEFT_ENDPOINT=http://localhost:8080 weft login

     The printed verify URL points at :8080 (the api host); change the
     host to :5173 (the frontend) before opening it in the browser
     where you're already signed in. Approve, and weft writes a JWT
     to ~/.weft.yaml.

  Web shell: while signed in, http://localhost:5173/shell talks to the
  same coordinator over HTTP and skips the device-code dance.
EOF
