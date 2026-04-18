#!/bin/bash
# Delete EBS volumes left over from the 2026-04-18 failed-attach leak.
# Target criteria, ALL required:
#   tag shard_tier   = <any>
#   tag shard_id     = pending-i-*            (placeholder id, never
#                                              rewritten by christen)
#   state            = available              (never successfully attached)
#
# Real shards carry their minted id (e.g. shard-20260418T0000-1) and
# live volumes are in `in-use`, so neither pattern matches — this
# script cannot touch them.
#
# Dry-run by default. Re-run with --apply once the list looks right.
#
# Usage:
#   ./scripts/cleanup-orphan-volumes.sh           # dry run: show + skip
#   ./scripts/cleanup-orphan-volumes.sh --apply   # actually delete

set -euo pipefail

REGION="us-east-2"
AWS="${AWS:-kevcons aws}"
APPLY=0

for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    us-*|eu-*|ap-*) REGION="$arg" ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

echo "── Orphan volume cleanup  [region=$REGION  apply=$APPLY] ─────────"
echo

# DescribeVolumes and filter client-side so we can inspect the exact
# predicate that decides delete vs. skip.
json=$($AWS ec2 describe-volumes \
  --region "$REGION" \
  --filters Name=tag-key,Values=shard_tier Name=status,Values=available \
  --query 'Volumes[].{id:VolumeId,state:State,az:AvailabilityZone,size:Size,tags:Tags}' \
  --output json)

orphans=$(echo "$json" | jq -r '
  .[]
  | select(
      (.tags // [])
      | map(select(.Key=="shard_id") | .Value)
      | .[0]? // ""
      | startswith("pending-")
    )
  | "\(.id)\t\(.state)\t\(.az)\t\(.size)GB\t\((.tags // []) | map(select(.Key=="shard_id") | .Value) | .[0] // "<none>")"
')

if [ -z "$orphans" ]; then
  echo "no orphans — nothing to do."
  exit 0
fi

echo "Will delete these volumes:"
echo
printf "  %-22s %-10s %-14s %-6s %s\n" "VolumeId" "State" "AZ" "Size" "shard_id tag"
echo "$orphans" | awk -F'\t' '{printf "  %-22s %-10s %-14s %-6s %s\n", $1,$2,$3,$4,$5}'
echo

count=$(echo "$orphans" | wc -l | tr -d ' ')
echo "Total: $count volumes."
echo

if [ "$APPLY" != "1" ]; then
  echo "(dry run — re-run with --apply to actually delete)"
  exit 0
fi

# Extra sanity gate: refuse to delete more than 100 volumes in one
# run. If the leak ever recurs worse than 2026-04-18, we want a human
# in the loop.
if [ "$count" -gt 100 ]; then
  echo "refusing to delete $count volumes in one run — bump the cap" >&2
  exit 3
fi

echo "$orphans" | awk -F'\t' '{print $1}' | while read -r vol; do
  echo "deleting $vol"
  $AWS ec2 delete-volume --region "$REGION" --volume-id "$vol"
done

echo
echo "done."
