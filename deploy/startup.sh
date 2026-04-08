#!/bin/bash
# Weft startup script — reads the instance Role tag, attaches persistent
# EBS for coordinator, and starts the appropriate systemd service.

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

# For the coordinator: attach persistent EBS and fetch config from SSM.
if [ "$ROLE" = "coordinator" ]; then

  # Attach persistent EBS volume for config store
  VOLUME_ID=$(aws ssm get-parameter \
    --name "/$APP_NAME/config/coordinator-ebs-volume-id" \
    --region "$REGION" \
    --query 'Parameter.Value' --output text 2>/dev/null || echo "")

  if [ -n "$VOLUME_ID" ] && [ "$VOLUME_ID" != "None" ]; then
    echo "Attaching EBS volume $VOLUME_ID..."

    # Wait for volume to be available. If stuck in-use on a dead instance,
    # force-detach it.
    for i in $(seq 1 60); do
      STATE=$(aws ec2 describe-volumes --volume-ids "$VOLUME_ID" --region "$REGION" \
        --query 'Volumes[0].State' --output text 2>/dev/null || echo "unknown")
      if [ "$STATE" = "available" ]; then
        break
      fi

      # If in-use, check if it's attached to a different (dead) instance
      if [ "$STATE" = "in-use" ]; then
        ATTACHED_TO=$(aws ec2 describe-volumes --volume-ids "$VOLUME_ID" --region "$REGION" \
          --query 'Volumes[0].Attachments[0].InstanceId' --output text 2>/dev/null || echo "")
        if [ -n "$ATTACHED_TO" ] && [ "$ATTACHED_TO" != "$INSTANCE_ID" ]; then
          echo "Volume attached to stale instance $ATTACHED_TO — force-detaching..."
          aws ec2 detach-volume --volume-id "$VOLUME_ID" --force --region "$REGION" 2>/dev/null || true
          sleep 10
          continue
        fi
      fi

      echo "Volume state: $STATE, waiting..."
      sleep 5
    done

    aws ec2 attach-volume \
      --volume-id "$VOLUME_ID" \
      --instance-id "$INSTANCE_ID" \
      --device /dev/xvdf \
      --region "$REGION" 2>/dev/null || echo "Volume attach failed (may already be attached)"

    # Wait for device to appear
    for i in $(seq 1 15); do
      if [ -b /dev/xvdf ] || [ -b /dev/nvme1n1 ]; then
        break
      fi
      sleep 2
    done

    # Determine actual device name (NVMe instances use nvme1n1)
    DEVICE="/dev/xvdf"
    [ -b /dev/nvme1n1 ] && DEVICE="/dev/nvme1n1"

    # Format if new (no filesystem)
    if ! blkid "$DEVICE" >/dev/null 2>&1; then
      echo "Formatting new volume..."
      mkfs.ext4 "$DEVICE"
    fi

    # Mount
    mkdir -p /opt/app/data
    mount "$DEVICE" /opt/app/data
    chown app:app /opt/app/data
    echo "Mounted $VOLUME_ID at /opt/app/data"
  fi

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

  cat > /opt/app/coordinator.env <<EOF
WEFT_SHARD_ASG_SMALL=${SHARD_ASG_SMALL}
WEFT_SHARD_ASG_MEDIUM=${SHARD_ASG_MEDIUM}
WEFT_SHARD_ASG_LARGE=${SHARD_ASG_LARGE}
WEFT_COMPUTE_ASG=${COMPUTE_ASG}
WEFT_REGION=${REGION}
WEFT_S3_REGION=${REGION}
WEFT_PARQUET_DIR=${PARQUET_DIR}
WEFT_LAMBDA_FUNCTION_URL=${LAMBDA_FUNCTION_URL}
EOF
  chown app:app /opt/app/coordinator.env
fi

systemctl start "weft-$ROLE"
