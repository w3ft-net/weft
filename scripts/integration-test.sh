#!/bin/bash
# One-shot wrapper for CI: build + start the docker stack, run integration
# tests, then tear it down. For local development, just `docker compose up
# -d --build` once and run `go test -tags integration ./integration/`
# directly — the harness applies migrations and checks readiness itself.
set -euo pipefail

cd "$(dirname "$0")/.."

cleanup() {
    echo "=== Tearing down docker stack ==="
    docker compose down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Building + starting docker stack ==="
docker compose up -d --build

# Give shards a moment to heartbeat and get adopted before tests run.
# (The harness handles coordinator readiness, but shard adoption happens
# asynchronously after that.)
echo "=== Waiting for shard adoption ==="
sleep 15

echo "=== Running integration tests ==="
go test -tags integration ./integration/ -v -count=1 -timeout 120s
