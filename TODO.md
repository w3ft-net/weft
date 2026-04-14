# Remaining work to get dogfooding working in prod

## Verified working
- Docker: weftd → coordinator → local shard → query returns data ✓
- Provisioner bootstraps shard with current time window ✓
- Heartbeat RPC defined and coordinator handler implemented ✓
- Loud failure: CRITICAL/DROPPING logs when no shards ✓
- kevcons deploy now refreshes all ASGs (coordinator + shard + compute) ✓

## Prod blockers (in order)

### 1. SSM parameter for coordinator-nlb
`kevcons config set` wrote to the JSON blob at `/weft/config/app`. The shard startup script reads a standalone parameter at `/weft/config/coordinator-nlb`. Need to either:
- Add it as a Terraform variable in the weft stack (preferred — version controlled)
- Or create a standalone SSM parameter manually

The value is the internal NLB DNS: `weft-nlb-*.elb.us-east-2.amazonaws.com:9020`

### 2. Verify shard instance refresh completed
The `kevcons deploy` fires shard refresh as fire-and-forget. Need to confirm:
- Shard instances got the new AMI
- Startup script ran and generated `/opt/app/shard.env`
- weft-shard service started with `--coordinator-addr`
- Heartbeat goroutine is running

Can verify by SSHing to a shard instance (need to fix `kevcons ssh --role worker` for weft stack — it looks for a proxy ASG that doesn't exist).

### 3. Heartbeat migration on docker-compose postgres
The Docker postgres doesn't have the `shard_heartbeats` table. The provisioner logs `heartbeat query failed: relation does not exist`. For docker dev this is non-blocking (local launcher bootstraps directly) but should be fixed for completeness.

Options:
- Run migration in docker-compose init
- Or have the coordinator create the table if it doesn't exist (non-partitioned fallback for dev)

### 4. Shard startup script: `date -d` doesn't work on Debian
The startup script uses `date -u -d "+8 hours"` for WINDOW_END, which is GNU date syntax. Debian's date may not support `-d`. Need to test or use a portable alternative.

## Non-blocking improvements

### 5. kevcons ssh for shard/compute roles
`kevcons ssh --role worker` tries to find a proxy bastion ASG (`weft-proxy`) which doesn't exist. Need to either:
- Add direct SSM session support (no bastion needed — instances have SSM agent)
- Or fix the role routing for the weft stack

### 6. Terraform: coordinator-nlb variable
Add `coordinator_nlb_addr` as an output from the weft Terraform stack and write it to a standalone SSM parameter that shard instances read.

### 7. Heartbeat-based dead shard detection
The provisioner reads heartbeats and adopts live shards, but doesn't yet remove dead ones. Need:
- On each check tick, compare registry entries against heartbeats
- Mark shards with no heartbeat in 2+ minutes as dead
- Remove dead shards from registry
- Log: `provisioner: shard X dead (no heartbeat for Ns)`

### 8. Ring buffer drain monitoring
The drain goroutine should log periodically:
- How many rows drained per tick
- Which shard(s) received them
- If drain is failing (shard write errors)

### 9. Remove ASGLauncher from provisioner bootstrap path
The provisioner still tries `ASGLauncher.LaunchInstance` in prod bootstrap. With heartbeat adoption, bootstrap should just wait for shards to appear via heartbeats rather than scaling the ASG. The ASG scaling should be controlled by CloudWatch alarms or the provisioner signaling desired capacity based on throughput, not by the bootstrap path.
