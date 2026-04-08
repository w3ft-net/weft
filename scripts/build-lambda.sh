#!/bin/bash
# Build the Lambda cold shard binary and package as a zip.
# Output: /tmp/weft-lambda-shard.zip
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building lambda-shard via Docker (arm64)..."
docker build -f Dockerfile.lambda -t weft-lambda-shard ..

echo "Extracting binary from image..."
CONTAINER=$(docker create weft-lambda-shard)
docker cp "$CONTAINER:/var/runtime/bootstrap" /tmp/bootstrap
docker rm "$CONTAINER" > /dev/null

echo "Packaging zip..."
cd /tmp
zip -j weft-lambda-shard.zip bootstrap
rm bootstrap

echo "Built /tmp/weft-lambda-shard.zip"
ls -lh /tmp/weft-lambda-shard.zip
