#!/bin/bash
# Weft startup script — reads the instance Role tag, fetches config from
# SSM, and starts the appropriate systemd services.

set -euo pipefail

# Get instance metadata
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Read the Role tag
ROLE=$(aws ec2 describe-tags \
  --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=Role" \
  --region "$REGION" \
  --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

if [ -z "$ROLE" ] || [ "$ROLE" = "None" ]; then
  echo "No Role tag found on instance $INSTANCE_ID, defaulting to coordinator"
  ROLE="coordinator"
fi

echo "Starting weft-$ROLE on instance $INSTANCE_ID in $REGION"
APP_NAME="weft"

# For the coordinator: fetch config from SSM.
if [ "$ROLE" = "coordinator" ]; then

  # Database DSN from SSM (PostgreSQL on RDS).
  DB_DSN=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/db-dsn" \
    --region "$REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  # Fetch ASG names from SSM (tiered shard ASGs)
  SHARD_ASG_SMALL=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/shard-asg-small" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  SHARD_ASG_MEDIUM=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/shard-asg-medium" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  SHARD_ASG_LARGE=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/shard-asg-large" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  # Backward compat: fall back to single shard-asg-name
  if [ -z "$SHARD_ASG_SMALL" ]; then
    SHARD_ASG_SMALL=$(aws ssm get-parameter \
      --name "/$APP_NAME/config/shard-asg-name" \
      --region "$REGION" \
      --query 'Parameter.Value' --output text 2>/dev/null || echo "")
  fi

  COMPUTE_ASG=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/compute-asg-name" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  # Cold storage config
  PARQUET_DIR=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/parquet-dir" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  LAMBDA_FUNCTION_URL=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/lambda-function-url" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  # API server config
  JWT_SECRET=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/jwt-secret" \
    --region "$REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  STRIPE_SECRET_KEY=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/stripe-secret-key" \
    --region "$REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  STRIPE_WEBHOOK_SECRET=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/stripe-webhook-secret" \
    --region "$REGION" \
    --with-decryption \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  EMAIL_HOST_PASSWORD=$(aws secretsmanager get-secret-value \
    --secret-id "/$APP_NAME/EMAIL_HOST_PASSWORD" \
    --region "$REGION" \
    --query 'SecretString' --output text 2>/dev/null || echo "")

  SLACK_CLIENT_SECRET=$(aws secretsmanager get-secret-value \
    --secret-id "/$APP_NAME/SLACK_CLIENT_SECRET" \
    --region "$REGION" \
    --query 'SecretString' --output text 2>/dev/null || echo "")

  SLACK_CLIENT_ID=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/slack-client-id" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  cat > /opt/app/coordinator.env <<EOF
WEFT_DB_DSN=${DB_DSN}
WEFT_SHARD_ASG_SMALL=${SHARD_ASG_SMALL}
WEFT_SHARD_ASG_MEDIUM=${SHARD_ASG_MEDIUM}
WEFT_SHARD_ASG_LARGE=${SHARD_ASG_LARGE}
WEFT_COMPUTE_ASG=${COMPUTE_ASG}
WEFT_REGION=${REGION}
WEFT_S3_REGION=${REGION}
WEFT_PARQUET_DIR=${PARQUET_DIR}
WEFT_LAMBDA_FUNCTION_URL=${LAMBDA_FUNCTION_URL}
JWT_SECRET=${JWT_SECRET}
STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY}
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET}
STRIPE_PRICE_SWATCH=price_1TK7iK2SENTbrh5us0SI1nRz
STRIPE_PRICE_YARD=price_1TK7jI2SENTbrh5uP3wVxnlx
STRIPE_PRICE_BOLT=price_1TK7kH2SENTbrh5upDvBswnG
EMAIL_HOST_PASSWORD=${EMAIL_HOST_PASSWORD}
SLACK_CLIENT_ID=${SLACK_CLIENT_ID}
SLACK_CLIENT_SECRET=${SLACK_CLIENT_SECRET}
EOF
  chown app:app /opt/app/coordinator.env
fi

# For shard roles: generate shard.env with self-configuration.
if [ "$ROLE" = "shard" ]; then
  # Coordinator NLB address from SSM.
  COORD_NLB=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/coordinator-nlb" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  cat > /opt/app/shard.env <<EOF
SHARD_ID=${INSTANCE_ID}
COORDINATOR_ADDR=${COORD_NLB:-localhost:9020}
EOF
  chown app:app /opt/app/shard.env
  echo "shard env: id=$INSTANCE_ID coordinator=$COORD_NLB"
fi

systemctl start "weft-$ROLE"

# Also start the API server on coordinator hosts.
if [ "$ROLE" = "coordinator" ]; then
  systemctl start weft-api
fi

# Start weftd on all roles if a token is available.
WEFT_AGENT_TOKEN=$(aws secretsmanager get-secret-value \
  --secret-id "/$APP_NAME/WEFT_AGENT_TOKEN" \
  --region "$REGION" \
  --query 'SecretString' --output text 2>/dev/null || echo "")

if [ -n "$WEFT_AGENT_TOKEN" ] && command -v weftd >/dev/null 2>&1; then
  mkdir -p /etc/weftd
  cat > /etc/weftd/config.yaml <<WEFTCFG
endpoint: ingest.w3ft.net:9020
token: ${WEFT_AGENT_TOKEN}
sources:
  - path: /opt/app/logs/*.log
    metrics:
      enabled: true
      interval: 30s
WEFTCFG
  chmod 600 /etc/weftd/config.yaml

  # Create systemd unit if not already present.
  if [ ! -f /etc/systemd/system/weftd.service ]; then
    cat > /etc/systemd/system/weftd.service <<'UNIT'
[Unit]
Description=weftd log agent
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/weftd --config=/etc/weftd/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
  fi

  systemctl enable weftd
  systemctl restart weftd
  echo "weftd started for role=$ROLE"
fi
