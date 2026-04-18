#!/bin/bash
# One-screen snapshot of the shard lifecycle pipeline. Re-runnable as a
# watch loop without drowning — all output fits in ~15 lines at steady
# state, and only expands details when something interesting is
# happening (pending hooks, non-empty queue, recent coordinator log).
#
# Usage:
#   ./scripts/lifecycle-status.sh           # one-shot
#   ./scripts/lifecycle-status.sh --verbose # full diagnostic (old behaviour)
#   watch -n 10 ./scripts/lifecycle-status.sh

set -euo pipefail

REGION="us-east-2"
APP_NAME="weft"
VERBOSE=0

for arg in "$@"; do
  case "$arg" in
    --verbose|-v) VERBOSE=1 ;;
    us-east-*|us-west-*|eu-*|ap-*) REGION="$arg" ;;
    *)            REGION="$arg" ;;
  esac
done

AWS="${AWS:-kevcons aws}"

# ── SQS depth ──────────────────────────────────────────────
QUEUE_URL=$($AWS ssm get-parameter \
  --region "$REGION" \
  --name "/$APP_NAME/config/shard-lifecycle-queue-url" \
  --query 'Parameter.Value' --output text 2>/dev/null || echo "")
SQS_DEPTH="?"
SQS_INFLIGHT="?"
if [[ "$QUEUE_URL" =~ ^https ]]; then
  attrs=$($AWS sqs get-queue-attributes \
    --region "$REGION" \
    --queue-url "$QUEUE_URL" \
    --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
    --query 'Attributes' --output json 2>/dev/null || echo "{}")
  SQS_DEPTH=$(echo "$attrs" | jq -r '.ApproximateNumberOfMessages // "?"')
  SQS_INFLIGHT=$(echo "$attrs" | jq -r '.ApproximateNumberOfMessagesNotVisible // "?"')
fi

# ── ASG state (one row per tier) ──────────────────────────
# Build a compact `tier  inService/desired  pending`.
asg_row() {
  local asg="$1"
  local json
  json=$($AWS autoscaling describe-auto-scaling-groups \
    --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query 'AutoScalingGroups[0]' --output json 2>/dev/null || echo "null")
  if [ "$json" = "null" ]; then
    printf "  %-20s %s\n" "$asg" "(not found)"
    return
  fi
  local desired inservice pending
  desired=$(echo "$json" | jq -r '.DesiredCapacity // 0')
  inservice=$(echo "$json" | jq -r '[.Instances[]? | select(.LifecycleState=="InService")] | length')
  pending=$(echo "$json" | jq -r '[.Instances[]? | select(.LifecycleState=="Pending:Wait")] | length')
  local flag=""
  [ "$pending" != "0" ] && flag=" ⚠ PENDING"
  printf "  %-20s %s/%s InService, %s Pending%s\n" "$asg" "$inservice" "$desired" "$pending" "$flag"
}

# ── Volumes (compact) ──────────────────────────────────────
# One line per volume: shard_id  state  az  vol-id
volumes_summary() {
  local json
  json=$($AWS ec2 describe-volumes \
    --region "$REGION" \
    --filters Name=tag-key,Values=shard_tier \
    --query 'Volumes[].{id:VolumeId, state:State, az:AvailabilityZone, size:Size, tags:Tags}' \
    --output json 2>/dev/null || echo "[]")
  local count
  count=$(echo "$json" | jq -r 'length')
  if [ "$count" = "0" ]; then
    echo "  (no shard volumes)"
    return
  fi
  local pending_count
  pending_count=$(echo "$json" | jq -r '[.[] | select(.tags[]? | select(.Key=="shard_id") | .Value | startswith("pending-"))] | length')
  local avail_count
  avail_count=$(echo "$json" | jq -r '[.[] | select(.state=="available")] | length')
  printf "  %d total, %d in-use, %d available, %d with pending-id\n" \
    "$count" "$(( count - avail_count ))" "$avail_count" "$pending_count"
  if [ "$VERBOSE" = "1" ] || [ "$pending_count" != "0" ] || [ "$avail_count" != "0" ]; then
    echo "$json" | jq -r '.[] |
      "    " +
      (.id | tostring) + "  " +
      (.state | tostring) + "  " +
      (.az | tostring) + "  " +
      ((.tags[]? | select(.Key=="shard_id") | .Value) // "<untagged>")'
  fi
}

# ── Recent coordinator log (SSM) ──────────────────────────
# Only show 5 most-recent lifecycle/christen/retire lines, unless
# verbose.
coord_log() {
  local coord_id
  coord_id=$($AWS autoscaling describe-auto-scaling-groups \
    --region "$REGION" \
    --auto-scaling-group-names "weft-coordinator" \
    --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' \
    --output text 2>/dev/null || echo "")
  if [ -z "$coord_id" ] || [ "$coord_id" = "None" ]; then
    echo "  no InService coordinator"
    return
  fi
  local cmd_id
  local lines="5"
  [ "$VERBOSE" = "1" ] && lines="40"
  cmd_id=$($AWS ssm send-command \
    --region "$REGION" \
    --instance-ids "$coord_id" \
    --document-name AWS-RunShellScript \
    --parameters "commands=[\"journalctl -u weft-coordinator --no-pager -n 500 2>/dev/null | grep -E 'lifecycle:|christened|retir' | tail -$lines\"]" \
    --query 'Command.CommandId' --output text 2>/dev/null || echo "")
  if [ -z "$cmd_id" ]; then
    echo "  (SSM send-command failed)"
    return
  fi
  for _ in $(seq 1 12); do
    local state
    state=$($AWS ssm get-command-invocation \
      --region "$REGION" \
      --command-id "$cmd_id" --instance-id "$coord_id" \
      --query 'Status' --output text 2>/dev/null || echo "Pending")
    if [ "$state" = "Success" ] || [ "$state" = "Failed" ]; then
      break
    fi
    sleep 1
  done
  local out
  out=$($AWS ssm get-command-invocation \
    --region "$REGION" \
    --command-id "$cmd_id" --instance-id "$coord_id" \
    --query 'StandardOutputContent' --output text 2>/dev/null || echo "")
  if [ -z "$out" ] || [ "$out" = "None" ]; then
    echo "  (no recent lifecycle activity)"
  else
    echo "$out" | sed 's/^/    /'
  fi
}

# ── Output ────────────────────────────────────────────────
printf "weft lifecycle  %s  [%s]\n" "$(date -u +%H:%M:%SZ)" "$REGION"
printf "SQS: depth=%s inflight=%s\n" "$SQS_DEPTH" "$SQS_INFLIGHT"
echo "ASGs:"
asg_row weft-shard-small
asg_row weft-shard-medium
asg_row weft-shard-large
echo "Volumes:"
volumes_summary
echo "Coordinator log (last lifecycle/christen/retir lines):"
coord_log
