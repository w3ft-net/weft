#!/bin/bash
# Run the dockerized integration test suite.
#
# Usage: ./scripts/integration-test.sh
#
# This script:
#   1. Builds and starts the docker compose stack
#   2. Waits for the coordinator to be healthy
#   3. Runs go test -tags integration ./integration/
#   4. Tears down the stack regardless of test outcome
set -euo pipefail

cd "$(dirname "$0")/.."

cleanup() {
    echo "=== Tearing down docker stack ==="
    docker compose down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Building docker images ==="
docker compose build --quiet

echo "=== Starting docker stack ==="
docker compose up -d

echo "=== Waiting for coordinator health ==="
for i in $(seq 1 30); do
    if curl -sf http://localhost:9021/health > /dev/null 2>&1; then
        echo "Coordinator healthy after ${i}s"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: coordinator not healthy after 30s"
        docker compose logs coordinator | tail -20
        exit 1
    fi
    sleep 1
done

# Wait a bit more for shards to heartbeat and get adopted.
echo "=== Waiting for shard adoption ==="
sleep 15

echo "=== Running integration tests ==="
go test -tags integration ./integration/ -v -count=1 -timeout 120s
