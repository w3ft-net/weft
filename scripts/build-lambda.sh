#!/bin/bash
# Build the Lambda cold shard binary and package as a zip.
# Output: /tmp/weft-lambda-shard.zip
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building lambda-shard for linux/amd64..."
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o /tmp/bootstrap ./cmd/lambda-shard/

echo "Packaging zip..."
cd /tmp
zip -j weft-lambda-shard.zip bootstrap
rm bootstrap

echo "Built /tmp/weft-lambda-shard.zip"
ls -lh /tmp/weft-lambda-shard.zip
