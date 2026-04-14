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
    # Env vars: WEFT_DB_DSN, WEFT_REGION, WEFT_SHARD_ASG_SMALL,
    #   WEFT_SHARD_ASG_MEDIUM, WEFT_SHARD_ASG_LARGE, WEFT_COMPUTE_ASG,
    #   WEFT_PARQUET_DIR, WEFT_LAMBDA_FUNCTION_URL, WEFT_CUSTOMER
    ARGS="--port=${COORDINATOR_PORT:-9020} --health-port=${COORDINATOR_HEALTH_PORT:-9021}"

    [ -n "${WEFT_DB_DSN:-}" ]             && ARGS="$ARGS --db-dsn=$WEFT_DB_DSN"
    [ -n "${WEFT_REGION:-}" ]             && ARGS="$ARGS --region=$WEFT_REGION"
    [ -n "${WEFT_SHARD_ASG_SMALL:-}" ]    && ARGS="$ARGS --shard-asg-small=$WEFT_SHARD_ASG_SMALL"
    [ -n "${WEFT_SHARD_ASG_MEDIUM:-}" ]   && ARGS="$ARGS --shard-asg-medium=$WEFT_SHARD_ASG_MEDIUM"
    [ -n "${WEFT_SHARD_ASG_LARGE:-}" ]    && ARGS="$ARGS --shard-asg-large=$WEFT_SHARD_ASG_LARGE"
    [ -n "${WEFT_COMPUTE_ASG:-}" ]        && ARGS="$ARGS --compute-asg=$WEFT_COMPUTE_ASG"
    [ -n "${WEFT_CUSTOMER:-}" ]           && ARGS="$ARGS --customer=$WEFT_CUSTOMER"

    exec weft-coordinator $ARGS
    ;;

  shard)
    # Env vars: SHARD_ID, COORDINATOR_ADDR, HEARTBEAT_INTERVAL
    ARGS="--id=${SHARD_ID:?SHARD_ID required}"
    ARGS="$ARGS --grpc-port=${SHARD_GRPC_PORT:-9000}"
    ARGS="$ARGS --http-port=${SHARD_HTTP_PORT:-9001}"
    ARGS="$ARGS --db=${SHARD_DB_PATH:-/opt/app/data/shard.duckdb}"

    [ -n "${COORDINATOR_ADDR:-}" ]    && ARGS="$ARGS --coordinator-addr=$COORDINATOR_ADDR"
    [ -n "${HEARTBEAT_INTERVAL:-}" ]  && ARGS="$ARGS --heartbeat-interval=$HEARTBEAT_INTERVAL"

    exec weft-shard $ARGS
    ;;

  compute)
    ARGS="--port=${COMPUTE_PORT:-9010}"
    exec weft-compute $ARGS
    ;;

  api)
    # Env vars: WEFT_DB_DSN, COORDINATOR_ADDR, BASE_URL, JWT_SECRET,
    #   STRIPE_*, EMAIL_HOST_PASSWORD, SLACK_*
    ARGS="--port=${API_PORT:-8080}"

    [ -n "${COORDINATOR_ADDR:-}" ]  && ARGS="$ARGS --coordinator=$COORDINATOR_ADDR"
    [ -n "${WEFT_DB_DSN:-}" ]       && ARGS="$ARGS --db-dsn=$WEFT_DB_DSN"
    [ -n "${BASE_URL:-}" ]          && ARGS="$ARGS --base-url=$BASE_URL"

    exec weft-api $ARGS
    ;;

  *)
    echo "Unknown ROLE: $ROLE"
    exit 1
    ;;
esac
