#!/bin/bash
# Unified entrypoint for all weft roles.
#
# Used by both Docker (env vars set directly) and production AMIs
# (startup.sh reads SSM into the same env vars, then execs this script).
#
# Required env var: ROLE (coordinator, shard, compute, api)
# Each role reads its own env vars — see below.

set -euo pipefail

ROLE="${ROLE:?ROLE env var is required}"

case "$ROLE" in
  coordinator)
    ARGS=(--port="${COORDINATOR_PORT:-9020}" --health-port="${COORDINATOR_HEALTH_PORT:-9021}")

    [ -n "${DB_DSN:-}" ]                  && ARGS+=(--db-dsn="$DB_DSN")
    [ -n "${WEFT_REGION:-}" ]             && ARGS+=(--region="$WEFT_REGION")
    [ -n "${WEFT_SHARD_ASG_SMALL:-}" ]    && ARGS+=(--shard-asg-small="$WEFT_SHARD_ASG_SMALL")
    [ -n "${WEFT_SHARD_ASG_MEDIUM:-}" ]   && ARGS+=(--shard-asg-medium="$WEFT_SHARD_ASG_MEDIUM")
    [ -n "${WEFT_SHARD_ASG_LARGE:-}" ]    && ARGS+=(--shard-asg-large="$WEFT_SHARD_ASG_LARGE")
    [ -n "${WEFT_COMPUTE_ASG:-}" ]        && ARGS+=(--compute-asg="$WEFT_COMPUTE_ASG")
    [ -n "${WEFT_CUSTOMER:-}" ]           && ARGS+=(--customer="$WEFT_CUSTOMER")

    exec weft-coordinator "${ARGS[@]}"
    ;;

  shard)
    # Identity is sourced from <data-dir>/shard.json, written by the
    # coordinator via the Configure RPC on first christening and then
    # persisted across restarts on the EBS volume.
    ARGS=(--grpc-port="${SHARD_GRPC_PORT:-9000}")
    ARGS+=(--http-port="${SHARD_HTTP_PORT:-9001}")
    ARGS+=(--data-dir="${SHARD_DATA_DIR:-/opt/app/data}")

    [ -n "${COORDINATOR_ADDR:-}" ]        && ARGS+=(--coordinator-addr="$COORDINATOR_ADDR")
    [ -n "${HEARTBEAT_INTERVAL:-}" ]      && ARGS+=(--heartbeat-interval="$HEARTBEAT_INTERVAL")

    exec weft-shard "${ARGS[@]}"
    ;;

  compute)
    ARGS=(--port="${COMPUTE_PORT:-9010}")
    exec weft-compute "${ARGS[@]}"
    ;;

  api)
    ARGS=(--port="${API_PORT:-8080}")

    [ -n "${COORDINATOR_ADDR:-}" ]  && ARGS+=(--coordinator="$COORDINATOR_ADDR")
    [ -n "${DB_DSN:-}" ]            && ARGS+=(--db-dsn="$DB_DSN")
    [ -n "${BASE_URL:-}" ]          && ARGS+=(--base-url="$BASE_URL")

    exec weft-api "${ARGS[@]}"
    ;;

  *)
    echo "Unknown ROLE: $ROLE"
    exit 1
    ;;
esac
