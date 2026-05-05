#!/bin/bash
# Collapse the current store/migrations/* into store/schema.sql.
#
# Deterministic: spins up a transient postgres, applies the existing
# schema.sql (if non-empty), runs the embedded weft-migrate against it
# (which applies every file in migrations/), then dumps the resulting
# schema + goose_db_version ledger as the new schema.sql. Migration
# files are deleted because their effects are now baked in.
#
# No remote DB read, no kevcons, no AWS credentials. Works the same
# locally and in CI. The only required tools are docker and go.
#
# Usage: ./scripts/collapse-migrations.sh
#   (or `make collapse` from the repo root)

set -euo pipefail

PROJECT="weft-collapse-$$"
IMAGE="weft-collapse-postgres"
PORT=5434
DSN="postgres://weft:weft@localhost:${PORT}/weft?sslmode=disable"
SCHEMA_FILE="store/schema.sql"
MIGRATIONS_DIR="store/migrations"

cleanup() {
  docker rm -f "$PROJECT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The dev/prod stack uses Dockerfile.postgres (postgres:16 + pg_partman
# + pg_cron). Migrations 002+ require the partman extension, so we
# build the same image rather than pulling a stock postgres.
echo "==> building postgres image with pg_partman + pg_cron"
docker build -t "$IMAGE" -f Dockerfile.postgres . >/dev/null

echo "==> spinning up transient postgres on :${PORT} (project=${PROJECT})"
docker run -d --rm --name "$PROJECT" \
  -e POSTGRES_USER=weft \
  -e POSTGRES_PASSWORD=weft \
  -e POSTGRES_DB=weft \
  -p "${PORT}:5432" \
  "$IMAGE" \
  postgres -c shared_preload_libraries=pg_cron -c cron.database_name=weft >/dev/null

echo "==> waiting for postgres ready"
for i in $(seq 1 60); do
  if docker exec "$PROJECT" pg_isready -U weft >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$PROJECT" pg_isready -U weft >/dev/null 2>&1 || {
  echo "postgres never became ready"
  exit 1
}

if [ -s "$SCHEMA_FILE" ] && grep -qE "^[^-]" "$SCHEMA_FILE"; then
  echo "==> applying existing $SCHEMA_FILE"
  docker exec -i "$PROJECT" psql -v ON_ERROR_STOP=1 -U weft -d weft < "$SCHEMA_FILE"
else
  echo "==> $SCHEMA_FILE is empty/comment-only — fresh collapse"
fi

echo "==> running weft-migrate to apply current migrations/"
go run ./cmd/weft-migrate -db-dsn "$DSN"

echo "==> dumping new $SCHEMA_FILE"
{
  cat <<'EOF'
-- store/schema.sql is the collapsed snapshot of the production
-- schema. Regenerate with `make collapse` after a successful prod
-- deploy. Applied automatically to a virgin DB before goose runs the
-- migrations/ directory; goose then sees the collapsed migrations
-- already recorded in goose_db_version and only applies post-
-- collapse deltas.
--
-- See docs/plans/migration-collapse.md for the design.

EOF
  docker exec "$PROJECT" pg_dump \
    --schema-only --no-owner --no-acl --no-comments \
    -U weft weft
  # Seed-bearing tables: dump --data-only so future collapses don't
  # silently drop seeds that past migrations inserted. `buckets` has
  # the day-1 'default' row from migration 020. Add tables here as
  # new migrations introduce seed data.
  docker exec "$PROJECT" pg_dump \
    --data-only --table=public.buckets --inserts \
    --no-owner --no-acl \
    -U weft weft
  docker exec "$PROJECT" pg_dump \
    --data-only --table=public.goose_db_version --inserts \
    --no-owner --no-acl \
    -U weft weft
} > "${SCHEMA_FILE}.new"
mv "${SCHEMA_FILE}.new" "$SCHEMA_FILE"

echo "==> removing collapsed migration files from $MIGRATIONS_DIR/"
# shellcheck disable=SC2046  # globbing is intentional
rm -f "$MIGRATIONS_DIR"/*.sql

# Keep the directory present so //go:embed migrations stays valid.
touch "$MIGRATIONS_DIR/.gitkeep"

echo
echo "==> collapse complete"
echo "    Review:   git diff -- $SCHEMA_FILE $MIGRATIONS_DIR"
echo "    Commit:   git add -A $SCHEMA_FILE $MIGRATIONS_DIR && \\"
echo "              git commit -m 'store: collapse migrations through prod high-water mark'"
