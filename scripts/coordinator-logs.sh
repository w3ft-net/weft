#!/bin/bash
# Pull weft-coordinator service status + logs from whichever instance in
# the weft-coordinator ASG is currently InService. Useful when new
# instances are passing the lifecycle hook but failing ELB health checks.
#
# Usage: ./scripts/coordinator-logs.sh [region]

set -euo pipefail

REGION="${1:-us-east-2}"
ASG="weft-coordinator"
OUT_FILE="/tmp/weft-coordinator-logs-$(date -u +%Y%m%dT%H%M%SZ).log"

INST=$(kevcons aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$ASG" \
    --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService` || LifecycleState==`Pending`].InstanceId | [0]' \
    --output text)

if [ -z "$INST" ] || [ "$INST" = "None" ]; then
    echo "no InService/Pending instance in $ASG" >&2
    exit 1
fi
echo "== inspecting $INST =="

# Write the diagnostic commands to a JSON params file, which avoids the
# fragile SSM shorthand syntax when any individual command has quotes.
PARAMS_FILE=$(mktemp -t weft-coord-logs-params.XXXXXX.json)
trap 'rm -f "$PARAMS_FILE"' EXIT

python3 - <<'PY' > "$PARAMS_FILE"
import json
script = r"""
echo =========== systemctl status weft-coordinator ===========
systemctl status weft-coordinator --no-pager -l || true
echo =========== journalctl -u weft-coordinator ===========
journalctl -u weft-coordinator --no-pager -n 80 || true
echo =========== systemctl status weft-startup ===========
systemctl status weft-startup --no-pager -l || true
echo =========== journalctl -u weft-startup ===========
journalctl -u weft-startup --no-pager -n 40 || true
echo =========== /opt/app/coordinator.env keys ===========
if [ -f /opt/app/coordinator.env ]; then
    awk -F= '{print $1"="}' /opt/app/coordinator.env
else
    echo MISSING
fi
echo =========== /opt/app/.env keys ===========
if [ -f /opt/app/.env ]; then
    awk -F= '{print $1"="}' /opt/app/.env
else
    echo MISSING
fi
echo =========== curl localhost:9021/health ===========
curl -s -m 3 localhost:9021/health || echo not-responding
echo =========== tail /opt/app/logs/coordinator.log ===========
tail -n 60 /opt/app/logs/coordinator.log 2>/dev/null || echo "no log file"
"""
print(json.dumps({"commands": [script]}))
PY

CMD_ID=$(kevcons aws ssm send-command --region "$REGION" \
    --instance-ids "$INST" \
    --document-name AWS-RunShellScript \
    --parameters "file://$PARAMS_FILE" \
    --query 'Command.CommandId' --output text)
echo "   command: $CMD_ID"

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
    echo "# weft-coordinator diagnostics"
    echo "# instance: $INST"
    echo "# captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    kevcons aws ssm get-command-invocation --region "$REGION" \
        --command-id "$CMD_ID" --instance-id "$INST" \
        --query 'StandardOutputContent' --output text
    echo
    echo "== StandardErrorContent =="
    kevcons aws ssm get-command-invocation --region "$REGION" \
        --command-id "$CMD_ID" --instance-id "$INST" \
        --query 'StandardErrorContent' --output text
} > "$OUT_FILE"

echo "== wrote $OUT_FILE ($(wc -l <"$OUT_FILE") lines) =="
