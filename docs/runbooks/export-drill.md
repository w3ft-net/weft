# Archive export drill

A manual run of one real export, on a scratch S3 prefix, with the feature still
off. It proves the pieces that have only ever run against fakes, and it produces
the numbers that replace the placeholder instance-size table. Part of
`docs/plans/ephemeral-archive-export.md` (rollout checklist, section 4).

Run it **before** `archive_export_enabled` is flipped. With the export enabled,
the sweeper treats a `drill-…` resource as an orphan (no shard row) and removes it
15 minutes after launch.

All AWS calls use `kevcons aws` (it assumes into the weft account, `220664822695`).
The values below were read from the live account on 2026-10-06.

## 0. Gate: the AMI must contain the export code

The export host runs `weft-shard export`, `deploy/export-role.sh` and
`weft-export.service`, which exist only in an AMI built after the branch lands on
`main`. The current AMI (`ami-09a1b088bd30ef493`, built 2026-10-05, GitSHA
`74c379f`) predates them.

```bash
AMI=$(kevcons aws ssm get-parameter --name /weft/ami/latest --query Parameter.Value --output text)
echo $AMI                     # must NOT be ami-09a1b088bd30ef493
kevcons aws ec2 describe-images --image-ids $AMI \
  --query "Images[0].Tags[?Key=='GitSHA'||Key=='ShardGitSHA'].[Key,Value]" --output text
```

The `GitSHA` tag must be the merged tip or a descendant of it:
`git merge-base --is-ancestor <merged tip> <GitSHA>` in the weft repo.

## 1. Variables

```bash
LT=lt-0f377613f0b339c8f                        # weft-export-lt (Role=export)
SHARD=shard-20260921T0000-4a672d0d             # the shard whose export has been failing
VOL=vol-0102bf2642b979383                      # its data volume, 100 GiB, us-east-2a
SUBNET=subnet-056a72712195c8c15                # the shard's subnet (us-east-2a, S3 gateway endpoint)
BUCKET=weft-cold-220664822695
DRILL=drill-4a672d0d                           # NEVER the real shard id
PREFIX=s3://$BUCKET/_export-drill              # scratch; never the real archive prefix
```

## 2. Snapshot the shard's volume

A snapshot of an in-use volume is a read-only operation on the live shard. Do not
tag it `export_shard_id`, so no sweep treats it as an export snapshot.

```bash
SNAP=$(kevcons aws ec2 create-snapshot --volume-id $VOL \
  --description "archive export drill $DRILL" \
  --tag-specifications "ResourceType=snapshot,Tags=[{Key=Name,Value=export-drill}]" \
  --query SnapshotId --output text)
kevcons aws ec2 wait snapshot-completed --snapshot-ids $SNAP
```

## 3. Launch one export host

This is what the coordinator itself sends (`ExportResources.LaunchInstance`): the
export template, a per-attempt instance type, the snapshot as a block device
mapping (the volume is created and attached at launch and deleted with the
instance), terminate-on-shutdown, and the `export_*` tags the host reads from IMDS.
**Do not add a `shard_id` or `shard_tier` tag.**

Start with `r7g.xlarge`. A launch `--dry-run` of everything except the snapshot
mapping succeeded on 2026-10-06 (template, subnet, tags, instance type, and the
role's permissions).

```bash
IID=$(kevcons aws ec2 run-instances \
  --launch-template "LaunchTemplateId=$LT,Version=\$Latest" \
  --image-id $AMI --instance-type r7g.xlarge --subnet-id $SUBNET \
  --block-device-mappings "DeviceName=/dev/sdf,Ebs={SnapshotId=$SNAP,VolumeType=gp3,DeleteOnTermination=true}" \
  --instance-initiated-shutdown-behavior terminate \
  --tag-specifications \
    "ResourceType=instance,Tags=[{Key=Name,Value=$DRILL},{Key=Role,Value=export},{Key=export_shard_id,Value=$DRILL},{Key=export_attempt,Value=1},{Key=export_prefix,Value=$PREFIX}]" \
    "ResourceType=volume,Tags=[{Key=export_shard_id,Value=$DRILL}]" \
  --query "Instances[0].InstanceId" --output text)
echo $IID
```

## 4. Watch it

```bash
kevcons aws ec2 describe-instances --instance-ids $IID \
  --query "Reservations[0].Instances[0].State.Name" --output text      # pending → running → … → terminated
```

The SSM agent takes a minute or two after boot. Then read the host's own logs:

```bash
CMD=$(kevcons aws ssm send-command --instance-ids $IID --document-name AWS-RunShellScript \
  --parameters 'commands=["tail -n 80 /opt/app/logs/export.log","echo ---","tail -n 40 /var/log/user-data.log","echo ---","lsblk","df -h /mnt/export"]' \
  --query Command.CommandId --output text)
kevcons aws ssm get-command-invocation --command-id $CMD --instance-id $IID \
  --query StandardOutputContent --output text
```

weftd should also ship the export log into weft under `/log/weft/export/…`
(`deployment=export`); if it does not appear there, that is a finding.

Expected, in order: the volume appears at `/dev/sdf` and mounts at `/mnt/export`;
`archive attempt=1 start`, `stats`, `sizing`, `copy start`, periodic
`progress phase=copy` lines, `copy done`, `done`; `export … done`; the host powers
off and the instance goes `terminated`.

## 5. Verify the result

```bash
kevcons aws s3 ls $PREFIX/ --recursive --human-readable --summarize | tail -20
kevcons aws s3 cp $PREFIX/_manifests/$DRILL.json - | jq '{attempt, rows, db_bytes, parquet_path, customers: (.customers|length)}'
```

- [ ] The manifest exists, `attempt` is 1, and `rows` equals the shard's `row_count`
      (`SELECT row_count FROM shard_records WHERE id = '$SHARD'`; the shard is still
      ingesting nothing, so the two should match).
- [ ] Output is under `$PREFIX/$DRILL.attempt-1/…` (hive partitions) or
      `$PREFIX/$DRILL.attempt-1.parquet`, and nowhere else.
- [ ] The instance is `terminated` and its restored volume is gone:
      `kevcons aws ec2 describe-volumes --filters Name=tag:export_shard_id,Values=$DRILL`
      returns nothing.
- [ ] It did **not** act as a shard:
      `SELECT count(*) FROM shard_heartbeats WHERE instance_id = '$IID'` is 0, and the
      live shard `$SHARD` is unaffected (same addr and instance in `shard_records`).
- [ ] The live shard kept serving queries throughout (the snapshot must not have
      disturbed it).

## 6. Capture the numbers

From the `archive attempt=1 …` lines in the host's `export.log`:

| What | Where | Value |
|---|---|---|
| Instance type | launch | |
| `.duckdb` bytes | `sizing db_bytes=` | |
| Estimated Parquet bytes, target files | `sizing est_parquet_bytes=`, `target_files=` | |
| Granularity | `sizing granularity=` | |
| Rows | `copy done rows=` | |
| Stats scan time | `stats … took=` | |
| COPY time | `copy done … took=` | |
| Total wall time | `done … total=` | |
| Peak RSS (kB) | max `rss_kb=` over `progress` lines | |
| Peak swap (kB) | max `swap_kb=` | |
| Peak memory PSI (full, 10 s) | max `psi_full10=` | |
| Peak spill bytes | max `spill_bytes=` | |
| Snapshot to running host | timestamps | |

PSI `full` near 0 and no swap means the instance had room; sustained PSI or swap
means it was too small. Run it again one class smaller (`r7g.large`) to find the
edge. These replace `defaultExportSizing` in `logdb/shard_export.go`, which is a
seed (there is no env override yet).

## 7. Failure drills

**a. A host that dies mid-export.** Launch as in step 3 (attempt 2, a fresh
`export_attempt` so the output lands in `$DRILL.attempt-2`), wait until
`copy start` appears, then:

```bash
kevcons aws ec2 terminate-instances --instance-ids <new instance id>
```

- [ ] No manifest is written for attempt 2 (a manifest is the proof of completion).
- [ ] Partial output may exist under `$DRILL.attempt-2`; delete it by hand (the
      coordinator would, once enabled).

**b. A host with no restored volume.** Launch as in step 3 but **omit
`--block-device-mappings`** (and use `export_attempt` 3). `export-role.sh` waits
about 60 s for `/dev/sdf`, fails, and its EXIT trap powers the host off.

- [ ] The host terminates itself within a few minutes.
- [ ] `/var/log/user-data.log` (or the console output) says `restored volume
      never appeared at /dev/sdf`, and no manifest exists. This is the
      shutdown-on-failure guarantee, the thing that stops a broken host from
      lingering and costing money.

## 8. Clean up

```bash
kevcons aws ec2 delete-snapshot --snapshot-id $SNAP
kevcons aws s3 rm $PREFIX/ --recursive
kevcons aws ec2 describe-instances --filters Name=tag:export_shard_id,Values=$DRILL \
  --query "Reservations[].Instances[].[InstanceId,State.Name]" --output text    # all terminated
kevcons aws ec2 describe-volumes --filters Name=tag:export_shard_id,Values=$DRILL \
  --query "Volumes[].VolumeId" --output text                                    # none
```

## Stop and investigate if

- the host registers or heartbeats as a shard (it must not: that is the identity
  collision the whole `Role=export` / `/mnt/export` design exists to prevent);
- the volume is formatted (`mkfs` must never run on a restored volume);
- the manifest's row count differs from the shard's `row_count`;
- the host is still running 10 minutes after the export finished.
