#!/bin/bash
# Weft startup script — reads the instance Role tag and starts the
# appropriate systemd service with config from SSM.

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

# For the coordinator, fetch ASG names from SSM and write an env file
if [ "$ROLE" = "coordinator" ]; then
  APP_NAME="weft"

  SHARD_ASG=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/shard-asg-name" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  COMPUTE_ASG=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/compute-asg-name" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  # Write environment file for the coordinator systemd unit
  cat > /opt/app/coordinator.env <<EOF
WEFT_SHARD_ASG=${SHARD_ASG}
WEFT_COMPUTE_ASG=${COMPUTE_ASG}
WEFT_REGION=${REGION}
EOF
  chown app:app /opt/app/coordinator.env
fi

systemctl start "weft-$ROLE"
