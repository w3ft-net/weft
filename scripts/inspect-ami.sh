#!/bin/bash
# Launch a throwaway EC2 instance from a weft AMI, read key files via SSM
# Run Command, terminate. Used to verify what a built AMI actually
# contains when the git SHA tag and observed behaviour disagree.
#
# Needs the weft-instance-profile (grants SSM) to exist in the target
# account — it does for any stack this script would sensibly inspect.
#
# Usage:  ./scripts/inspect-ami.sh <ami-id> [region]

set -euo pipefail

AMI_ID="${1:?usage: $0 <ami-id> [region]}"
REGION="${2:-us-east-2}"
PROFILE="${WEFT_INSTANCE_PROFILE:-weft-instance-profile}"

echo "== picking a default subnet in $REGION =="
SUBNET=$(kevcons aws ec2 describe-subnets --region "$REGION" \
    --filters Name=default-for-az,Values=true \
    --query 'Subnets[0].SubnetId' --output text)
echo "   subnet: $SUBNET"

echo "== detecting AMI architecture =="
ARCH=$(kevcons aws ec2 describe-images --region "$REGION" --image-ids "$AMI_ID" \
    --query 'Images[0].Architecture' --output text)
case "$ARCH" in
    arm64)   INSTANCE_TYPE=t4g.micro ;;
    x86_64)  INSTANCE_TYPE=t3.micro  ;;
    *)       echo "unknown AMI architecture: $ARCH" >&2; exit 1 ;;
esac
echo "   arch: $ARCH -> $INSTANCE_TYPE"

echo "== launching throwaway instance from $AMI_ID =="
INST=$(kevcons aws ec2 run-instances --region "$REGION" \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --subnet-id "$SUBNET" \
    --iam-instance-profile "Name=$PROFILE" \
    --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=weft-ami-inspect}]' \
    --query 'Instances[0].InstanceId' --output text)
echo "   instance: $INST"

cleanup() {
    echo "== terminating $INST =="
    kevcons aws ec2 terminate-instances --region "$REGION" --instance-ids "$INST" \
        --query 'TerminatingInstances[0].CurrentState.Name' --output text >/dev/null || true
}
trap cleanup EXIT

echo "== waiting for $INST to register with SSM (up to 3m) =="
for i in $(seq 1 36); do
    STATE=$(kevcons aws ssm describe-instance-information --region "$REGION" \
        --filters "Key=InstanceIds,Values=$INST" \
        --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || echo "")
    if [ "$STATE" = "Online" ]; then
        echo "   SSM registered after ~$((i*5))s"
        break
    fi
    sleep 5
done
if [ "${STATE:-}" != "Online" ]; then
    echo "ERROR: instance never registered with SSM" >&2
    exit 1
fi

echo "== sending inspection command via SSM =="
CMD_ID=$(kevcons aws ssm send-command --region "$REGION" \
    --instance-ids "$INST" \
    --document-name AWS-RunShellScript \
    --parameters 'commands=[
        "echo =========== /opt/app/scripts/fetch-config.sh ===========",
        "cat /opt/app/scripts/fetch-config.sh",
        "echo =========== ls /opt/app/bin/ ===========",
        "ls -la /opt/app/bin/",
        "echo =========== ls /usr/local/bin/weft* ===========",
        "ls -la /usr/local/bin/weft* 2>/dev/null || echo no matches",
        "echo =========== ls /etc/systemd/system/weft-*.service ===========",
        "ls -la /etc/systemd/system/weft-*.service 2>/dev/null || echo no matches"
    ]' \
    --query 'Command.CommandId' --output text)
echo "   command: $CMD_ID"

echo "== waiting for command to finish =="
for i in $(seq 1 24); do
    STATUS=$(kevcons aws ssm get-command-invocation --region "$REGION" \
        --command-id "$CMD_ID" --instance-id "$INST" \
        --query 'Status' --output text 2>/dev/null || echo "")
    case "$STATUS" in
        Success|Failed|Cancelled|TimedOut) break ;;
    esac
    sleep 5
done

echo
echo "== command output =="
kevcons aws ssm get-command-invocation --region "$REGION" \
    --command-id "$CMD_ID" --instance-id "$INST" \
    --query 'StandardOutputContent' --output text
