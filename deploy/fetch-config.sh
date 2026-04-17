#!/bin/bash
# Synchronous SSM fetch for values needed by the ASG lifecycle hook
# (migration command). Writes /opt/app/.env with just enough env for
# /usr/local/bin/weft-migrate — currently WEFT_DB_DSN.
#
# The service-level env (shard ASGs, region, secrets, etc.) continues to
# be written by startup.sh into /opt/app/coordinator.env, which systemd
# loads via EnvironmentFile= for the weft-coordinator / weft-api units.
# Those services run after this script, so no race with the hook.
set -euo pipefail

APP_NAME="${1:-${APP_NAME:-weft}}"

# Derive region from instance metadata if not already set (user-data
# exports AWS_REGION, but be defensive in case this is run manually).
if [ -z "${AWS_REGION:-}" ]; then
    TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
    AWS_REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
        http://169.254.169.254/latest/meta-data/placement/region)
fi

ENV_FILE="/opt/app/.env"

echo "fetch-config: writing $ENV_FILE"

WEFT_DB_DSN=$(aws ssm get-parameter \
    --name "/${APP_NAME}/config/db-dsn" \
    --region "$AWS_REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text)

umask 077
cat > "$ENV_FILE" <<EOF
# Generated at $(date -u +%Y-%m-%dT%H:%M:%SZ) by fetch-config.sh
APP_NAME=$APP_NAME
AWS_REGION=$AWS_REGION
WEFT_DB_DSN='$WEFT_DB_DSN'
EOF
chown app:app "$ENV_FILE"

echo "fetch-config: done"
