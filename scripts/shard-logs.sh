#!/bin/bash
# Pull weft-shard service status + logs from every InService instance in
# the shard ASGs (small/medium/large). Uses SSM Run Command directly
# against each shard instance — no tunneling through the coordinator, no
# bastion-key dependency. Works as long as the shards have SSM agent and
# outbound egress (NAT via coordinator).
#
# Usage: ./scripts/shard-logs.sh [region]

set -euo pipefail

REGION="${1:-us-east-2}"
ASGS=(weft-shard-small weft-shard-medium weft-shard-large)
OUT_FILE="/tmp/weft-shard-logs-$(date -u +%Y%m%dT%H%M%SZ).log"

# Build the per-instance diagnostic script. SSM's shorthand parameters
# syntax is fragile with quotes; passing JSON via file:// is reliable.
PARAMS_FILE=$(mktemp -t weft-shard-logs-params.XXXXXX.json)
trap 'rm -f "$PARAMS_FILE"' EXIT

python3 - <<'PY' > "$PARAMS_FILE"
import json
script = r"""
echo =========== hostname ===========
hostname
cat /etc/os-release 2>/dev/null | head -2 || true
echo =========== systemctl status weft-shard ===========
systemctl status weft-shard --no-pager -l || true
echo =========== journalctl -u weft-shard ===========
journalctl -u weft-shard --no-pager -n 120 || true
echo =========== tail /opt/app/logs/shard.log ===========
tail -n 80 /opt/app/logs/shard.log 2>/dev/null || echo "no log file"
echo =========== ss listen ports ===========
ss -tlnp 2>/dev/null | head -20 || netstat -tlnp 2>/dev/null | head -20 || true
echo =========== dmesg tail ===========
sudo dmesg | tail -40 2>/dev/null || true
echo =========== memory ===========
free -m
echo =========== disk ===========
df -h /
"""
print(json.dumps({"commands": [script]}))
PY

: > "$OUT_FILE"
{
    echo "# weft-shard diagnostics"
    echo "# captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
} >> "$OUT_FILE"

for ASG in "${ASGS[@]}"; do
    echo "== $ASG =="
    INST_IDS=$(kevcons aws autoscaling describe-auto-scaling-groups --region "$REGION" \
        --auto-scaling-group-names "$ASG" \
        --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService` || LifecycleState==`Pending`].InstanceId' \
        --output text 2>/dev/null || echo "")
    if [ -z "$INST_IDS" ]; then
        echo "   (no InService instances)"
        {
            echo "================================================================"
            echo "# $ASG — no InService instances"
            echo "================================================================"
        } >> "$OUT_FILE"
        continue
    fi

    for INST in $INST_IDS; do
        echo "   $INST — sending command"
        CMD_ID=$(kevcons aws ssm send-command --region "$REGION" \
            --instance-ids "$INST" \
            --document-name AWS-RunShellScript \
            --parameters "file://$PARAMS_FILE" \
            --query 'Command.CommandId' --output text 2>/dev/null || echo "")
        if [ -z "$CMD_ID" ]; then
            echo "      (ssm send-command failed — instance not SSM-registered?)"
            {
                echo "================================================================"
                echo "# $ASG / $INST — SSM send-command failed"
                echo "================================================================"
            } >> "$OUT_FILE"
            continue
        fi

        for i in $(seq 1 24); do
            STATUS=$(kevcons aws ssm get-command-invocation --region "$REGION" \
                --command-id "$CMD_ID" --instance-id "$INST" \
                --query 'Status' --output text 2>/dev/null || echo "")
            case "$STATUS" in
                Success|Failed|Cancelled|TimedOut) break ;;
            esac
            sleep 3
        done

        {
            echo "================================================================"
            echo "# $ASG / $INST (status: ${STATUS:-unknown})"
            echo "================================================================"
            kevcons aws ssm get-command-invocation --region "$REGION" \
                --command-id "$CMD_ID" --instance-id "$INST" \
                --query 'StandardOutputContent' --output text 2>/dev/null || echo "(no stdout)"
            echo
            echo "-- stderr --"
            kevcons aws ssm get-command-invocation --region "$REGION" \
                --command-id "$CMD_ID" --instance-id "$INST" \
                --query 'StandardErrorContent' --output text 2>/dev/null || echo "(no stderr)"
            echo
        } >> "$OUT_FILE"
    done
done

echo "== wrote $OUT_FILE ($(wc -l <"$OUT_FILE") lines) =="
