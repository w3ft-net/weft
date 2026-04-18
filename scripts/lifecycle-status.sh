#!/bin/bash
# Snapshot the shard lifecycle flow — intended for post-deploy verification
# of the EBS-volume rollout (docs/plans/ebs-shard-volumes.md).
#
# Reports:
#   - SQS queue depth + recent receives (poller health)
#   - ASG activity for each shard tier (lifecycle hook firing)
#   - EC2 volumes tagged for the weft fleet (what's been created/recovered)
#   - Shard instances and their attached non-root volumes
#   - Latest coordinator log lines mentioning "lifecycle:" or
#     "christened" (via SSM — no SSH needed)
#
# Usage: ./scripts/lifecycle-status.sh [region]

set -euo pipefail

REGION="${1:-us-east-2}"
APP_NAME="weft"

section() { printf '\n── %s ─────────────────────────────────────────\n' "$1"; }

section "SQS queue"
QUEUE_URL=$(aws ssm get-parameter \
  --region "$REGION" \
  --name "/$APP_NAME/config/shard-lifecycle-queue-url" \
  --query 'Parameter.Value' --output text 2>/dev/null || echo "")
if [ -z "$QUEUE_URL" ]; then
  echo "NO QUEUE URL in SSM — is terraform applied?"
else
  echo "queue: $QUEUE_URL"
  aws sqs get-queue-attributes \
    --region "$REGION" \
    --queue-url "$QUEUE_URL" \
    --attribute-names ApproximateNumberOfMessages \
                      ApproximateNumberOfMessagesNotVisible \
                      ApproximateNumberOfMessagesDelayed \
    --query 'Attributes' --output table || true
fi

section "ASG activity (last 10 per tier)"
for asg in weft-shard-small weft-shard-medium weft-shard-large; do
  printf "\n▸ %s\n" "$asg"
  aws autoscaling describe-scaling-activities \
    --region "$REGION" \
    --auto-scaling-group-name "$asg" \
    --max-records 10 \
    --query 'Activities[].[StartTime,StatusCode,Cause]' \
    --output table 2>/dev/null || echo "  (ASG not found)"
done

section "Pending lifecycle actions (should be 0 during steady state)"
for asg in weft-shard-small weft-shard-medium weft-shard-large; do
  count=$(aws autoscaling describe-auto-scaling-groups \
    --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query 'AutoScalingGroups[0].Instances[?LifecycleState==`Pending:Wait`] | length(@)' \
    --output text 2>/dev/null || echo "0")
  printf "  %s → %s pending\n" "$asg" "$count"
done

section "EC2 volumes tagged for weft shards"
aws ec2 describe-volumes \
  --region "$REGION" \
  --filters Name=tag-key,Values=shard_tier \
  --query 'Volumes[].[VolumeId,State,AvailabilityZone,Size,Tags[?Key==`shard_id`]|[0].Value,Tags[?Key==`shard_tier`]|[0].Value]' \
  --output table || true

section "Shard instances + attached volumes"
for asg in weft-shard-small weft-shard-medium weft-shard-large; do
  printf "\n▸ %s\n" "$asg"
  ids=$(aws autoscaling describe-auto-scaling-groups \
    --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId' \
    --output text 2>/dev/null || echo "")
  if [ -z "$ids" ]; then
    echo "  (no InService instances)"
    continue
  fi
  # For each instance, list its block-device mappings so we can see
  # whether /dev/sdf (or the nvme-renamed equivalent) is attached.
  # shellcheck disable=SC2086  # word-splitting is intentional
  aws ec2 describe-instances \
    --region "$REGION" \
    --instance-ids $ids \
    --query 'Reservations[].Instances[].[InstanceId,PrivateIpAddress,BlockDeviceMappings[?contains(DeviceName,`sdf`) || contains(DeviceName,`xvdf`) || contains(DeviceName,`nvme1`)].[DeviceName,Ebs.VolumeId]]' \
    --output json 2>/dev/null | head -30 || true
done

section "Coordinator lifecycle log lines (latest via SSM)"
COORD_ID=$(aws autoscaling describe-auto-scaling-groups \
  --region "$REGION" \
  --auto-scaling-group-names "weft-coordinator" \
  --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId' \
  --output text 2>/dev/null | head -n1 || echo "")
if [ -z "$COORD_ID" ]; then
  echo "no InService coordinator"
else
  echo "coordinator: $COORD_ID"
  CMD_ID=$(aws ssm send-command \
    --region "$REGION" \
    --instance-ids "$COORD_ID" \
    --document-name AWS-RunShellScript \
    --parameters 'commands=["journalctl -u weft-coordinator --no-pager -n 500 | grep -E \"lifecycle|christened|retir\" | tail -40 || true"]' \
    --query 'Command.CommandId' --output text 2>/dev/null || echo "")
  if [ -n "$CMD_ID" ]; then
    # Poll up to ~15s for completion
    for _ in $(seq 1 15); do
      STATE=$(aws ssm get-command-invocation \
        --region "$REGION" \
        --command-id "$CMD_ID" \
        --instance-id "$COORD_ID" \
        --query 'Status' --output text 2>/dev/null || echo "Pending")
      if [ "$STATE" = "Success" ] || [ "$STATE" = "Failed" ]; then
        break
      fi
      sleep 1
    done
    aws ssm get-command-invocation \
      --region "$REGION" \
      --command-id "$CMD_ID" \
      --instance-id "$COORD_ID" \
      --query 'StandardOutputContent' --output text 2>/dev/null || true
  fi
fi

echo
echo "Done. Expected happy-path indicators:"
echo "  * SQS: ApproximateNumberOfMessages = 0 (steady state)"
echo "  * ASG: 0 pending lifecycle actions"
echo "  * Volumes: one per InService shard, state=in-use, shard_id=<real id> (not 'pending-...')"
echo "  * Each shard instance has a non-root volume attached"
echo "  * Coordinator log shows CHRISTEN or RECOVER lines and no ABANDON"
