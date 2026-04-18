# Verification: EBS shard volume rollout

Once-off checklist for the first prod deploy of gap #4 + #5.

## Before the deploy

Confirm the terraform apply landed:

```bash
aws ssm get-parameter --region us-east-2 \
  --name /weft/config/shard-lifecycle-queue-url \
  --query 'Parameter.Value' --output text
# Expect: https://sqs.us-east-2.amazonaws.com/220664822695/weft-shard-lifecycle

aws ssm get-parameters-by-path --region us-east-2 \
  --path /weft/config/ --query 'Parameters[?contains(Name, `volume`)].Name'
# Expect: shard-small-volume-gb, shard-medium-volume-gb,
#         shard-large-volume-gb, shard-volume-device

aws autoscaling describe-lifecycle-hooks --region us-east-2 \
  --auto-scaling-group-name weft-shard-small \
  --query 'LifecycleHooks[].LifecycleHookName'
# Expect: ["weft-shard-small-launch"]
```

If any of those are missing, re-run `terraform apply`.

## During the deploy

Kick off `kevcons deploy` (or whatever triggers the ASG refresh).
In another terminal, watch the pipeline end-to-end:

```bash
./scripts/lifecycle-status.sh          # snapshot every ~30s
```

The sequence to expect, in order, over ~5–10 minutes:

1. **Shard ASGs start rolling.** `describe-scaling-activities` shows
   "Launching a new EC2 instance" rows. The new instance enters
   `Pending:Wait` — this is the hook holding it.

2. **SQS queue depth ticks up.** `ApproximateNumberOfMessages` goes
   from 0 to ≥1 within ~30s of the instance entering Pending:Wait.

3. **Coordinator drains the queue.** Within another ~5s the depth
   drops back to 0. Coordinator logs show:
   ```
   lifecycle: CHRISTEN instance i-… with volume vol-… (tier=small az=us-east-2a)
   ```
   (Or RECOVER when reusing an orphan.)

4. **Volume appears in EC2.** A new `vol-…` in state `in-use`,
   tagged `shard_tier=small` + `shard_id=pending-i-…` (temporary
   until christening completes).

5. **Instance exits Pending:Wait.** `ASG activity` shows the
   instance transitioning to `InService`. The matching activity
   line reads "Lifecycle Action Completed" with CONTINUE.

6. **Shard boots, heartbeats empty id.** Coordinator logs show:
   ```
   provisioner: christened shard-YYYYMMDDThhmm-N at 10.0.0.…:9000
     (window …, volume=vol-…)
   ```

7. **Volume tag rewrites.** `describe-volumes` now shows the
   volume tagged `shard_id=shard-YYYYMMDDThhmm-N` (the real minted
   id, not `pending-…`).

8. **Shard ingests.** Any subsequent heartbeat from the shard
   carries the real id; writes/queries route normally.

## After the deploy

Final state the script should report:

- SQS: `ApproximateNumberOfMessages = 0`, `…NotVisible = 0`
- ASGs: 0 pending lifecycle actions per tier
- Volumes: one `in-use` volume per shard instance, each tagged
  with a real shard id (no `pending-…` leftovers)
- Each shard instance's `describe-instances` shows a non-root
  `sdf`/`xvdf`/`nvme1…` device in `BlockDeviceMappings`
- Coordinator log contains `christened` lines, no `ABANDON`

## Red flags and how to diagnose

| Symptom | Likely cause | Next step |
|---|---|---|
| Instance stuck in `Pending:Wait` > 5 min | Coordinator not consuming the queue | `./scripts/coordinator-logs.sh` — look for SQS errors or the "lifecycle: polling" line missing |
| ASG activity shows "ABANDON" | Handler decision tree failed | Coordinator log shows `lifecycle: decide … ambiguous recovery` or `create volume for new shard` error; surface and fix |
| SQS depth grows monotonically | Poller crashed | Coordinator logs for panic; restart the service via SSM |
| Volume created but not attached | AttachVolume timed out | Check volume state — if `available`, coordinator can retry manually: `aws ec2 attach-volume …` |
| Shard boots but `/opt/app/data` empty | startup.sh couldn't resolve `/dev/sdf` | SSM-run `ls /dev/nvme*` + `ebsnvme-id` on the instance; check that the attach completed before boot |
| Shard comes up, heartbeat has stale shard id | `shard.json` survived from a prior volume incarnation | Look at vol tags vs shard.json content; operator decides whether to keep or wipe |

## Rolling back

If the christen/recover decision tree is producing bad outcomes and
we need to back out:

1. `aws autoscaling suspend-processes` on the shard ASGs to stop
   new instances from being launched.
2. Revert the coordinator deploy (previous AMI/kevcons release)
   — older coordinator ignores the queue URL entirely.
3. `delete-lifecycle-hook` on each ASG, or let the previous apply's
   terraform destroy them.
4. Leave the SNS/SQS/volumes in place for diagnosis; delete
   terraform-destroy afterwards.

Shards without a hook proceed through Pending immediately and launch
without a data volume — back to the pre-S5 behavior. Running shards
with EBS volumes keep their volumes (just orphaned from any
coordinator logic that would manage them), so data is preserved for
later re-integration.
