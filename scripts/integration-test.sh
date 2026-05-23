#!/bin/bash
# One-shot wrapper for CI: build + start the docker stack (with the
# LocalStack overlay so AWS-SDK-touching tests have an endpoint to hit),
# run integration tests, then tear it down.
#
# For local development, run the same compose-up incantation once and
# call `go test -tags 'integration test' ./integration/` directly — the
# harness applies migrations and checks readiness itself. `make integration-clean`
# (or `clean_local_state` below) wipes the dev/shard-data subdirs the
# DockerLauncher leaves behind across `down -v` runs.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.localstack.yml)

# Wipe all dev state the docker stack accumulates. Run BEFORE up so a
# previous failed run can't contaminate this one with stale shard rows
# (postgres heartbeat tokens, archived parquet, …) or stale per-shard
# DuckDB subdirs that the DockerLauncher intentionally preserves
# across container kills.
#
# Idempotent: down -v on a stack that isn't running is a no-op; rm -rf
# on a missing directory is too.
clean_local_state() {
    docker compose "${COMPOSE_FILES[@]}" down -v --remove-orphans 2>/dev/null || true
    # DockerLauncher containers aren't part of the compose stack — they
    # were `docker run`-spawned by the coordinator. Sweep them too so
    # they don't accumulate across runs.
    docker ps -aq --filter "name=weft-shard-" | xargs -r docker rm -f >/dev/null 2>&1 || true
    rm -rf dev/shard-data
}

cleanup() {
    echo "=== Tearing down docker stack ==="
    docker compose "${COMPOSE_FILES[@]}" down -v --remove-orphans 2>/dev/null || true
    docker ps -aq --filter "name=weft-shard-" | xargs -r docker rm -f >/dev/null 2>&1 || true
    rm -rf dev/shard-data
}
trap cleanup EXIT

echo "=== Wiping prior dev state ==="
clean_local_state

echo "=== Building + starting docker stack (with localstack) ==="
docker compose "${COMPOSE_FILES[@]}" up -d --build

# Give shards a moment to heartbeat and get adopted before tests run.
# (The harness handles coordinator readiness, but shard adoption happens
# asynchronously after that.)
echo "=== Waiting for shard adoption ==="
sleep 15

echo "=== Running integration tests ==="
# Tag set must include `test`: some integration files (e.g. lambda_test.go)
# reference helpers defined in `integration && test`-tagged files (t0 in
# cold_storage_test.go), so a bare `-tags integration` fails to compile.
go test -tags 'integration test' ./integration/ -v -count=1 -timeout 120s
