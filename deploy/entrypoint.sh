#!/bin/bash
# One entrypoint for every role of the open-source image. ROLE is required:
# api, coordinator, compute or shard. The environment carries the settings.
set -euo pipefail

ROLE="${ROLE:?ROLE env var is required}"

case "$ROLE" in
  coordinator)
    ARGS=(--port="${COORDINATOR_PORT:-9020}" --health-port="${COORDINATOR_HEALTH_PORT:-9021}")
    [ -n "${COORDINATOR_LOCAL_ADDR:-}" ] && ARGS+=(--local-addr="$COORDINATOR_LOCAL_ADDR")
    [ -n "${DB_DSN:-}" ]                 && ARGS+=(--db-dsn="$DB_DSN")
    [ -n "${COMPUTE_ADDR:-}" ]           && ARGS+=(--compute="$COMPUTE_ADDR")
    [ -n "${SHARE_URL_BASE:-}" ]         && ARGS+=(--share-url-base="$SHARE_URL_BASE")
    exec weft-coordinator "${ARGS[@]}"
    ;;

  shard)
    ARGS=(--grpc-port="${SHARD_GRPC_PORT:-9000}" --http-port="${SHARD_HTTP_PORT:-9001}" --data-dir="${SHARD_DATA_DIR:-/opt/app/data}")
    [ -n "${COORDINATOR_ADDR:-}" ]   && ARGS+=(--coordinator-addr="$COORDINATOR_ADDR")
    [ -n "${HEARTBEAT_INTERVAL:-}" ] && ARGS+=(--heartbeat-interval="$HEARTBEAT_INTERVAL")
    exec weft-shard "${ARGS[@]}"
    ;;

  compute)
    # The open image's compute is built with -tags localreader: with no
    # ARCHIVE_READER it reads archives in-process with DuckDB (duckdb-local), which
    # takes compute's CPU and memory on a large scan. Set ARCHIVE_READER=duckdb-lambda
    # to read them through a Lambda instead.
    ARGS=(--port="${COMPUTE_PORT:-9010}")
    [ -n "${ARCHIVE_READER:-}" ] && ARGS+=(--archive-reader-default="$ARCHIVE_READER")
    exec weft-compute "${ARGS[@]}"
    ;;

  api)
    ARGS=(--port="${API_PORT:-8080}")
    [ -n "${COORDINATOR_ADDR:-}" ] && ARGS+=(--coordinator="$COORDINATOR_ADDR")
    [ -n "${DB_DSN:-}" ]           && ARGS+=(--db-dsn="$DB_DSN")
    [ -n "${BASE_URL:-}" ]         && ARGS+=(--base-url="$BASE_URL")
    [ -n "${API_BASE_URL:-}" ]     && ARGS+=(--api-base-url="$API_BASE_URL")
    exec weft-api "${ARGS[@]}"
    ;;

  *)
    echo "Unknown ROLE: $ROLE" >&2
    exit 1
    ;;
esac
