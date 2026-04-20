#!/bin/bash
# fetch-config.sh — populate /opt/app/.env for pre-service-boot steps
# (ASG migration lifecycle hook, /etc/default/weftd generation).
#
# Delegates to /usr/local/bin/load-app-env, which slurps the kevcons
# config blob (infra-derived vars like SHARD_ASG_SMALL, PARQUET_DIR)
# and /weft/ secrets (JWT_SECRET, Stripe keys, WEFTD_TOKEN, …) into
# /opt/app/.env. Then appends WEFT_DB_DSN, the only value weft needs
# that isn't in the blob or secrets — the DB DSN has a distinct SSM
# lifecycle (separate stack) and stays standalone.
#
# Callers: infrastructure-catalog/modules/asg-application/user-data*.tpl
# at instance launch, and /opt/app/scripts/reload-config.sh on config
# version bumps.
set -euo pipefail

APP_NAME="${1:-${APP_NAME:-weft}}"

# Derive region from instance metadata if not already set.
if [ -z "${AWS_REGION:-}" ]; then
    TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
    AWS_REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
        http://169.254.169.254/latest/meta-data/placement/region)
fi

if [ ! -x /usr/local/bin/load-app-env ]; then
    echo "fetch-config: /usr/local/bin/load-app-env not installed — AMI out of date"
    exit 1
fi

echo "fetch-config: running load-app-env for $APP_NAME"
APP_NAME="$APP_NAME" AWS_REGION="$AWS_REGION" /usr/local/bin/load-app-env

WEFT_DB_DSN=$(aws ssm get-parameter \
    --name "/${APP_NAME}/config/db-dsn" \
    --region "$AWS_REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

if [ -n "$WEFT_DB_DSN" ]; then
    # Append to (not overwrite) load-app-env's output so both its blob
    # + secrets AND our DSN are present in a single source-able file.
    echo "WEFT_DB_DSN='$WEFT_DB_DSN'" >> /opt/app/.env
else
    echo "fetch-config: warning — /${APP_NAME}/config/db-dsn empty or unavailable"
fi

echo "fetch-config: done"
