#!/bin/bash
# Weft startup script — reads the WEFT_ROLE instance tag and starts the
# appropriate systemd service. Called by weft-startup.service on boot.

set -euo pipefail

# Get instance ID and region from IMDS
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Read the role tag
ROLE=$(aws ec2 describe-tags \
  --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=Role" \
  --region "$REGION" \
  --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

if [ -z "$ROLE" ] || [ "$ROLE" = "None" ]; then
  echo "No WEFT_ROLE tag found on instance $INSTANCE_ID, defaulting to coordinator"
  ROLE="coordinator"
fi

echo "Starting weft-$ROLE on instance $INSTANCE_ID"
systemctl start "weft-$ROLE"
