#!/bin/bash
# Bring up the multi-customer demo:
#   1. ensure base stack (postgres, coordinator, etc.) is running
#   2. mint fresh agent tokens via weft-demo-seed (always; old tokens revoked)
#   3. start the demo service containers (acme: 6, globex: 6) and the Loom
#      producer (1 container running the same Go cmd/loom-producer as prod)
#
# Loom Inc is the populated org used by the onboarding tour — every
# signup gets a read-only membership in it. Unlike acme/globex, its streams
# come from the Go loom-producer, so they carry trace IDs.
#
#   ./dev/demo/up.sh

set -euo pipefail

cd "$(dirname "$0")/../.."  # repo root
REPO_ROOT="$(pwd)"

DSN="${DB_DSN:-host=localhost port=5433 user=weft password=weft dbname=weft sslmode=disable}"
TOKENS="dev/demo/tokens.env"
SEED_BIN="dev/demo/weft-demo-seed"

echo "→ starting postgres"
docker compose up -d postgres

echo "→ waiting for postgres to accept connections"
until docker compose exec -T postgres pg_isready -U weft -q 2>/dev/null; do
  sleep 1
done

echo "→ building seed CLI"
go build -o "${SEED_BIN}" ./cmd/weft-demo-seed

# Always re-mint tokens so they match the current DB state. Without
# this, a postgres volume recreate leaves tokens.env pointing at hashes
# that no longer exist, and every producer crash-loops on auth.
rm -f "${TOKENS}"

echo "→ seeding customers/users/tokens (before API starts)"
# A fresh dev DB is virgin (no goose_db_version table yet) so
# store.Migrate has to apply schema.sql via psql for bootstrap. That
# path is gated by WEFT_ALLOW_SCHEMA_BOOTSTRAP — production never
# hits it (the prod DB is always already bootstrapped), and we'd
# rather fail loud than silently restore a stale embedded snapshot
# if it ever did.
WEFT_ALLOW_SCHEMA_BOOTSTRAP=1 "./${SEED_BIN}" --db-dsn "${DSN}" --tokens-out "${TOKENS}"

echo "→ building weft-server image + starting coordinator, api, compute"
docker compose up -d --build compute coordinator api

echo "→ bringing up 13 demo services (12 acme + globex, plus the loom producer)"
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
  globex-notifier-staging globex-notifier-prod \
  loom-producer

cat <<'EOF'
→ done.

Logging in (the auth surface is HTTP-only — no email is sent in dev):

  1. Start the frontend dev server (separate terminal):
       cd frontend && npx vite --port 5173

  2. Open http://localhost:5173 and submit one of the seeded emails.
     Acme team:    alice@acme.test, bob@acme.test, carol@acme.test
     Globex team:  dana@globex.test, eli@globex.test, frida@globex.test
     Loom owner:   ops@loom.test    (Loom Inc is the org new signups
                                     get a read-only membership in via
                                     the onboarding flow.)
     New signup:   any other email — sign up via the dashboard. The
                   onboarding tour kicks in and walks Loom's data.

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
