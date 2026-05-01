#!/bin/bash
# End-to-end runner for the cover-tagged integration tests
# (everything gated by the `cover` build tag — `coverage_cover_test.go`,
# `direct_to_compute_op[125]_*` boundary checks, and the op1/op2 main
# tests that dial shard-0/shard-1 directly).
#
# Two passes are required because the boundary-coverage tests assert
# against per-service `coverage/<service>.out` profile files that only
# exist AFTER a coverage-instrumented run has torn down (so containers
# flush their counter files):
#
#   pass 1: stack up with cover overlay → run cover-tagged tests
#           (port-dialing tests pass; boundary tests skip cleanly
#           because no profiles exist yet)
#   teardown → containers flush GOCOVERDIR counters to ./coverage/<svc>/
#   coverage-report.sh → materializes coverage/<service>.out
#   pass 2: stack down → re-run only the boundary tests against the
#           fresh profiles
#
# The non-cover integration suite continues to live in
# scripts/integration-test.sh.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.cover.yml)
COVER_TAGS="integration,cover"
COVDIR="coverage"

cleanup() {
    echo "=== Tearing down docker stack ==="
    docker compose "${COMPOSE_FILES[@]}" down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

# Start clean. Stale per-service counter dirs and .out files from a
# prior run would either confuse coverage-report.sh's per-service
# detection or feed pass 1's boundary tests profiles that no longer
# match the current source tree.
echo "=== Cleaning $COVDIR/ ==="
rm -rf "$COVDIR"
mkdir -p "$COVDIR"/{coordinator,shard-0,shard-1,compute,api}

echo "=== Building + starting docker stack with cover overlay ==="
docker compose "${COMPOSE_FILES[@]}" up -d --build

echo "=== Waiting for shard adoption ==="
sleep 15

# Pass 1: run the full cover-tagged suite. The port-9000-needing tests
# (op1/op2 main, and the op5 main test isn't here — it's plain
# integration) exercise the live cover-instrumented containers; the
# boundary tests skip because no .out files exist yet.
echo "=== Pass 1: cover-tagged integration tests ==="
go test -tags "$COVER_TAGS" ./integration/ -v -count=1 -timeout 5m

# Tear down NOW, in the middle, so containers flush counters into the
# bind-mounted ./coverage/<svc>/ dirs. The trap will run a second
# (no-op) tear-down on EXIT.
echo "=== Tearing down to flush coverage counters ==="
docker compose "${COMPOSE_FILES[@]}" down

# Materialize per-service .out files (and the merged report) under
# $COVDIR. Pass 2's boundary tests read from these.
echo "=== Materializing coverage profiles ==="
scripts/coverage-report.sh

# Pass 2: just the boundary checks. No docker stack needed — these
# tests are pure assertions over the on-disk profiles.
echo "=== Pass 2: boundary coverage checks against fresh profiles ==="
go test -tags "$COVER_TAGS" ./integration/ -v -count=1 -timeout 60s \
    -run '^(TestRequireRanIn_|TestDirectToCompute_Op[125]_BoundaryCoverage)$'
