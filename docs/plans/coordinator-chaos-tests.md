# Coordinator N=2 — chaos tests

Phase 4 of `docs/plans/coordinator-scaling-live-testing.md`: one test per
hazard/gap that `coordinator_desired = 2` (live in prod since 2026-09-19)
is supposed to have fixed. Modeled on `docs/plans/fsm-chaos-tests.md`'s
format — **(a)** action, **(b)** expected behavior, **(c)** PASS/FAIL
criteria. Run against prod directly, no isolated environment (Paul's
explicit call, see `docs/plans/coordinator-scaling-live-testing.md`).

Phase 3 logging (`evaluate_event`/`evaluate_tick`/`tailfanout_event`,
`store/telemetry_events.go`) shipped and was confirmed flowing in
`/log/weft/coordinator/main.log` on 2026-09-19 — tests 5, 6 and 4 lean on
it.

## Read this before running anything

**Gotcha A — `systemctl stop` / SIGTERM is *supposed to be* a clean
shutdown, not a crash — in practice it may not complete at all.**
`cmd/coordinator/main.go`'s SIGINT/SIGTERM handler calls
`GracefulStop()`, and every lease-holding loop's `ctx.Done()` branch is
*meant* to release its lease and log `lease_event ... event=released`
before exiting (`coordinator/nat.go`, `logdb/leader_election.go`) — but
those `ctx.Done()` branches only fire once `main()` returns and its
deferred cancels run, which can't happen until `GracefulStop()` itself
returns. Confirmed on 2026-09-19 (Test 1, attempt 2, see below and
`docs/known-issues.md`'s "`GracefulStop()` doesn't drain within 90s"):
`GracefulStop()` didn't return within systemd's 90s `TimeoutStopSec`, so
the unit was force-`SIGKILL`ed instead — meaning a "clean" stop can, in
practice, degrade into the same forced-kill path as `pkill -9`, just
much slower and via a `failed` unit state instead of a `released` lease
event. Don't assume `systemctl stop` is fast or clean; check
`journalctl` for how it actually ended.

An ungraceful kill remains the reliable way to test failure-of-the-holder
without waiting on `TimeoutStopSec`:
```
kevcons ssh --instance-id <id> --command 'sudo pkill -9 -f weft-coordinator'
```

`Restart=always` / `RestartSec=5` means systemd relaunches the same
process ~5s later as the *same* holder (holder = EC2 instance-id, stable
across process restarts) — that's a feature here, not a confound: it also
lets you watch the restarted process cleanly rejoin. **But** see Gotcha C
— on its own this doesn't test cross-instance failover at all.

**Gotcha B — poller leases have no reconcile loop; cron leases do.**
*(Historical — `coordinator/poller` no longer exists; poller leases can't
occur post-cutover. Left for context on Test 7's original findings.)*
`Poller.Start()` enumerates `log_groups` once at boot and never retries a
denied/lost lease. `CronExecutor` runs a 30s `reconcile()` ticker forever
and re-attempts anything not currently tracked. This means a poller
lease's holder dying is **not** automatically healed by the survivor —
see Test 7, which is likely to actually surface this as a fail, not a
pass.

**Gotcha C — a bare `pkill -9` doesn't test failover; it tests same-instance
crash recovery.** Confirmed running Test 1, attempt 1, on 2026-09-19 (see
results below and `docs/known-issues.md`): `Restart=always`/`RestartSec=5`
brings the process back in ~6s, and because lease holder identity is the
EC2 instance-id (not the process), the restarted process immediately
reclaims its own former leases via `AcquireLease`'s `holder = self`
branch — regardless of how long it was actually down — almost always
beating a genuinely healthy peer's own slower retry tick (10s for NAT/
shard-fsm-leader, 30s for cron).

**What actually works** (confirmed, attempt 2): plain
`sudo systemctl stop weft-coordinator` — no `mask` needed. Per Gotcha A,
`stop` doesn't reliably complete gracefully within 90s, and when it
doesn't, systemd force-kills it *without* triggering a restart (an
explicit stop's forced kill is still treated as "stop was requested,"
which suppresses `Restart=always` — unlike an out-of-the-blue `pkill -9`
on a process systemd didn't ask to stop, which does trigger a restart).
So a real holder outage happens, just ~90s slower than a bare kill would
give you. `mask` is only needed if you want to prevent this specific
mechanism from mattering — i.e. if `GracefulStop()` ever gets fixed to
return quickly, `stop` alone would then cleanly release+restart, and
you'd need `mask` again to hold the instance down. Belt-and-suspenders
version:
```
kevcons ssh --instance-id <id> --command 'sudo systemctl stop weft-coordinator && sudo systemctl mask weft-coordinator'
# ...run the test, observe failover...
kevcons ssh --instance-id <id> --command 'sudo systemctl unmask weft-coordinator && sudo systemctl start weft-coordinator'
```
Also worth knowing before you restore the instance: an outage long
enough to matter here (~90s+) is also long enough for the coordinator
ASG's own health check to mark the instance unhealthy and replace it
independently — confirmed in attempt 2, a fresh instance was already
`InService` before the old one finished terminating. Manually restarting
the old instance's service still works, but may be moot if the ASG has
already decided to replace it; check `describe-auto-scaling-groups`
before assuming your manual recovery is what's keeping the fleet at 2.

**No `kevcons` command mutates leases directly.** The only levers are (1)
kill/stop the holding process, or (2) `kevcons db query` against `leases`
directly — but (2) only creates a window of at most one `LeaseRenewInterval`
(10s) before a still-alive holder reclaims it on its own next tick, so it
is not a real substitute for (1) unless the holder is also down.

**Always pass `--instance-id` to `kevcons ssh`** once there are 2
coordinators — with `--command` and no `--instance-id`, it silently
picks `connectedInstanceIDs[0]` (SSM response order, not deterministic).

## Common building blocks

**Find the 2 coordinator instances:**
```
kevcons aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names weft-coordinator
# or
kevcons aws ec2 describe-instances --filters "Name=tag:Role,Values=coordinator" "Name=instance-state-name,Values=running"
```

**`leases` table** (`store/schema.sql`): `name` (PK), `holder` (= the
coordinator's own EC2 instance-id or `hostname-<8hex>` off-EC2 —
identical to the `coordinator_id` telemetry field, no separate lookup
needed), `expires_at`, `acquired_at` (preserved across same-holder
renewals — shows streak length, not last-renewal time). `LeaseTTL = 30s`,
`LeaseRenewInterval = 10s`. Names in play: `nat-holder`,
`shard-fsm-leader`, `poller:<log_groups.id>`, `cron:<cron_watches.id>`.

**DB snapshot** (before/after every test):
```sql
SELECT name, holder, expires_at, acquired_at FROM leases ORDER BY name;
```

**Live monitoring** (run one loop per instance — logs are per-host files,
not merged):
```
for id in <coord-a-id> <coord-b-id>; do
  ( while true; do
      kevcons ssh --instance-id "$id" --command \
        'sudo tail -n 300 /opt/app/logs/coordinator.log' \
        | grep -E "lease_event|evaluate_event|evaluate_tick|tailfanout_event"
      sleep 15
    done ) &
done
```

---

## Test 1 — NAT re-election

**Baseline**: `SELECT * FROM leases WHERE name = 'nat-holder';` — note the
current `holder`.

**Action**: take down the current `nat-holder` in a way that actually
tests cross-instance failover — per Gotcha C, a bare `pkill -9` doesn't:
`Restart=always`/`RestartSec=5` lets the same instance reclaim its own
leases via `AcquireLease`'s `holder = self` branch before a genuinely
healthy peer's slower retry tick ever gets a chance. Use the corrected
procedure instead ("What actually works" above):
```
kevcons ssh --instance-id <nat-holder-id> --command 'sudo systemctl stop weft-coordinator && sudo systemctl mask weft-coordinator'
# ...run the test, observe failover...
kevcons ssh --instance-id <nat-holder-id> --command 'sudo systemctl unmask weft-coordinator && sudo systemctl start weft-coordinator'
```

**Expected**:
1. Killed instance's `nat-holder` row sits unrenewed (no `released` event
   — it never got to run its shutdown path).
2. Once `expires_at` passes (worst case ≤ `LeaseTTL` + one
   `LeaseRenewInterval` ≈ 40s), the surviving coordinator's `NATElector`
   wins `AcquireLease` on its own next 10s tick.
3. Surviving coordinator logs `nat: acquired nat-holder lease, claiming
   route table...` and `lease_event lease=nat-holder holder=<new>
   event=acquired`, then calls `ReplaceRoute` (falling back to
   `CreateRoute`) against the NAT route table.
4. The stopped+masked instance stays down for the rest of the test
   (masking is what prevents `Restart=always` from relaunching it and
   reclaiming its own leases — see Gotcha C). Expect the coordinator
   ASG's own health check to notice the outage and start replacing the
   instance independently before you get to the unmask/restart step —
   that's a second, unrelated self-healing layer, not a test failure;
   just confirm the replacement rejoins as a non-leader challenger.

**PASS if**: `event=acquired` appears on the survivor within ~40s of the
kill, and the private-subnet route table's default route now points at
the survivor's ENI (`kevcons aws ec2 describe-route-tables --route-table-ids <rtb-id>`
— get the current id from the live `app_config_blob`, not the old
hardcoded value in `coordinator-horizontal-scaling.md`, which predates
this rollout).

**FAIL signals**: no `acquired` event within ~2×`LeaseTTL`; private-subnet
instances lose egress for non-S3 AWS API calls (SSM, CloudWatch Logs,
Secrets Manager — S3 itself is unaffected via the gateway endpoint from
item 1a).

**Result, attempt 1 (2026-09-19, bare `pkill -9`)**: ran against
`i-0d4b0c33ab6b07d29` (holder of `nat-holder`, `shard-fsm-leader`, and
33/36 cron leases at baseline). `pkill -9` confirmed via `journalctl`
(`code=killed, status=9/KILL`), systemd restarted the process 6s later.
Per Gotcha C, the restarted process reclaimed all of its own former
leases before the survivor (`i-03658aa89a13db588`) ever got a chance —
`nat-holder` and `shard-fsm-leader` never changed hands, no re-election
was actually exercised. No split-brain or double-fire occurred either
(mutual exclusion held correctly throughout). Route-table verification
was skipped since the holder never changed. Logged as a known issue in
`docs/known-issues.md`.

**Result, attempt 2 (2026-09-19, `systemctl stop`) — PASS.** Re-ran using
Gotcha C's procedure, but `sudo systemctl stop weft-coordinator` alone
was enough — the `&& systemctl mask` never even ran (see below) and
turned out not to be needed. Full timeline:
- `15:49:59` — stop requested.
- `15:51:29` — systemd's `TimeoutStopSec` (90s default) expired before
  `GracefulStop()` returned; systemd force-`SIGKILL`ed the process itself
  and marked the unit `failed`. **This is itself a new finding, not the
  intended one** — see `docs/known-issues.md`'s "GracefulStop doesn't
  drain within 90s" entry. It happened to still produce a real holder
  outage, which is what this test needed, so the run continued.
- `15:52:05`/`:06` — survivor logs `provisioner: acquired
  shard-fsm-leader lease` and `nat: acquired nat-holder lease, claiming
  route table rtb-08515c241db4539ec → eni-02687e33ab73e44cc`.
- Confirmed independently: `leases` table shows both on
  `i-03658aa89a13db588`; `describe-route-tables` confirms the live
  `0.0.0.0/0` route now targets that instance's ENI, state `active`;
  `evaluate_tick` lines resumed flowing from the new leader every minute
  from `15:52:42` onward with no gap or anomaly.
- Total failover time: ~2m6s from stop-request to lease acquisition —
  dominated entirely by the 90s forced-kill wait, not by the ~30-40s
  TTL/re-election mechanism itself, which behaved exactly as designed
  once the holder was actually gone.
- **Unplanned but relevant**: the coordinator ASG's own health check
  independently marked `i-0d4b0c33ab6b07d29` unhealthy during the outage
  and began replacing it (new instance `i-0408530e395c754a8` was already
  `InService` before the old one finished terminating) — a second,
  independent layer of self-healing on top of the lease-based failover
  this test targets. Manually `systemctl start`-ing the old instance's
  service (which did work, came back `active`) turned out to be moot:
  the ASG replaced it regardless. **For future runs targeting the
  coordinator ASG specifically: don't bother manually restarting the
  killed instance's service if the outage runs long — just let the ASG
  replace it, and verify the replacement joins cleanly as a non-leader.**

**Net verdict**: NAT re-election (this test) and shard-fsm-leader
re-election (Test 5) are both confirmed working correctly under a real
holder outage. Test 5 can be considered done by this same run — no need
to repeat it separately unless testing a mid-tick kill specifically
(this outage started between ticks, not mid-`Evaluate`).

## Test 2 — Poller double-ingest

**Moot as of the CloudWatch push cutover (`docs/plans/cloudwatch-kinesis-subscription.md`) — `coordinator/poller` was deleted entirely, so this scenario no longer applies to any current or future log group.** Left below as historical record of why it was skipped rather than run.

**Skipped 2026-09-19 — no live workload to test against.** Checked
`log_groups`: 30 rows, all `source_type IN ('weftd', 'direct')`, zero
`cloudwatch`. `Poller.Start()` currently enumerates nothing and never
spawns a `pollLoop`, and the `leases` table correspondingly has zero
`poller:*` rows. Nothing to kill. Revisit if a CloudWatch-sourced log
group is ever added; until then this scenario is blocked, not failed.
Skipped deliberately rather than fabricating a throwaway CloudWatch log
group just to exercise the path — the outcome is already predictable
from the code (no reconcile loop, see Gotcha B) with high confidence, and
Test 7 (scale down) is expected to surface the same underlying gap
incidentally if it ever does apply. Fix the `Poller` reconcile-loop gap
directly rather than manufacturing a workload to re-prove what reading
`coordinator/poller/poller.go` already shows.

**Baseline** (for whenever this becomes runnable):
```sql
SELECT lg.id, lg.app, lg.log_group_name, l.holder, l.expires_at
FROM log_groups lg LEFT JOIN leases l ON l.name = 'poller:' || lg.id
WHERE lg.enabled AND lg.source_type = 'cloudwatch';
```
Pick a log group and note its current `poller:<id>` holder.

**Action**: hard-kill that holder — a bare process kill, deliberately,
since this test wants the systemd auto-restart (unlike Test 1, this
isn't testing cross-instance failover; it's testing whether a restart's
`Start()` re-race recovers the lease at all, on either instance):
```
kevcons ssh --instance-id <id> --command 'sudo pkill -9 -f weft-coordinator'
```

**Expected**: per Gotcha B, `Poller.Start()` never retries — the *other*
coordinator's own `pollLoop` goroutine for this log group was already
denied at its own boot and returned permanently. The lease row goes
stale and **nothing re-acquires it** until some coordinator process
restarts from scratch (a redeploy, or this same kill causing a systemd
relaunch — which re-runs `Start()` and re-races for the lease).

**PASS if**: after the killed process's systemd auto-restart re-runs
`Start()`, exactly one coordinator (may be either) holds `poller:<id>`
again, and cross-checking ingested rows against the true CloudWatch event
count for the outage window shows no duplicates:
```
weft sh -c 'cat /log/<ns>/<svc> --since <t0> --until <t1> | sort | uniq -d'   # any output = duplicate
weft sh -c 'cat /log/<ns>/<svc> --since <t0> --until <t1> | wc -l'            # compare to:
kevcons aws logs filter-log-events --log-group-name <name> --start-time <ms> --end-time <ms> | jq '.events | length'
```

**FAIL signals**: any duplicate `(raw, ts)` pair; weft's count exceeds
CloudWatch's true count (double-ingest); OR — the more likely finding —
the lease sits orphaned indefinitely with no coordinator ever polling
that log group again until a full redeploy. Treat the latter as a real
gap to fix (add a reconcile loop to `Poller`, mirroring `CronExecutor`),
not as a test-harness artifact.

## Test 3 — Cron/watch double-fire

**Baseline**:
```sql
SELECT cw.id, cw.name, cw.schedule, l.holder, l.expires_at
FROM cron_watches cw LEFT JOIN leases l ON l.name = 'cron:' || cw.id
WHERE cw.enabled;
```

**Action**: per Gotcha C (learned in Test 1), use `sudo systemctl stop
weft-coordinator` on the holder, not a bare `pkill -9` — a raw kill just
lets the same instance's `Restart=always` reclaim its own cron leases in
~6s, well inside the 30s reconcile ticker's window, so it never actually
tests failover. `stop` reliably produces a real outage instead (it
doesn't drain cleanly — see `docs/known-issues.md`'s `GracefulStop`
entry — so expect it to take ~90s to actually die, not fast).

**Expected**: unlike poller, `CronExecutor.reconcile()` runs on a 30s
ticker forever. The stopped holder's `runWatch` goroutines die with it;
within ≤30s some other coordinator's `reconcile` retries `runWatch` for
that watch and wins the now-expired lease. "Some other coordinator" may
not be the pre-existing survivor: if the outage is long enough for the
coordinator ASG's own health check to replace the instance (see Test 1),
the *replacement* instance's first-boot `reconcile()` call (immediate,
not waiting for its own 30s ticker) can win the race before the
already-running survivor's next ticker fires.

**PASS if**: exactly one `cron_results` row for the watch's scheduled
fire time during the outage window:
```sql
SELECT id, cron_watch_id, ran_at, exit_code, duration_ms
FROM cron_results
WHERE cron_watch_id = <id> AND ran_at BETWEEN '<t0>' AND '<t1>'
ORDER BY ran_at;
```

**FAIL signals**: two `cron_results` rows for the same scheduled fire
(or two alert notifications) from two different coordinators' logs.

**Result (2026-09-19) — PASS.** Targeted `i-0408530e395c754a8`, holder of
`cron:31` (`coord-mem-by-host`, `*/2 * * * *`) and `cron:34`
(`lease-holders`, `*/2 * * * *`) — picked because they were the only two
crons *not* on the busy `nat-holder`/`shard-fsm-leader` instance, keeping
this test isolated from Test 1's. `systemctl stop` issued at `16:09:24`;
forced `SIGKILL` at `16:10:55` (91s — a second, near-identical
reproduction of the `GracefulStop` stall). Notably, the cron ticker kept
firing right up to the kill: the `16:10:42` scheduled run completed
normally (single row each, `exit_code=0`) 13s before the process died,
confirming `GracefulStop()` blocks new inbound work but doesn't halt
already-running background goroutines. The ASG replaced the instance
(`i-04fdbfb16e007e33f`, boot-started `16:12:24`); its first-boot
`reconcile()` won both leases one second later (`16:12:25`,
`lease_event ... event=acquired` for both) — beating `i-03658aa89a13db588`
(the long-running survivor, whose own 30s ticker hadn't fired yet) to
the acquisition. Only one `cron_results` row exists per watch for this window, timestamped
`16:12:25` — i.e. `runWatch` appears to execute once immediately on
winning a lease rather than waiting for the schedule's next natural
boundary (the prior steady-state fires were all at `:XX:42`; this one
broke that pattern). Not independently confirmed against `coordinator/
cron.go`'s source, but observationally: no duplicate rows at any point,
mutual exclusion held correctly through two consecutive coordinator
replacements.

## Test 4 — TailFanout under Redis disruption

**Prerequisite — verify before running**: the ElastiCache Valkey node
(`weft-redis`, single-node, no automatic failover) may not actually be
applied yet — `coordinator-horizontal-scaling.md` documented the
Terraform as written but not applied as of its last update. Confirm
first:
```
kevcons aws elasticache describe-replication-groups --replication-group-id weft-redis
```
If this 404s, this test is blocked on a `terraform apply` in
`infrastructure-live/prod/weft` first (protected IaC apply — human-run).

**Setup — pin one `tail -f` session to each coordinator** (the public
API endpoint round-robins across both, so this can't be done through the
normal client path):
```
aws ssm start-session --target <coord-a-id> --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["18080"]}'
aws ssm start-session --target <coord-b-id> --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["18081"]}'

WEFT_ENDPOINT=http://localhost:18080 weft sh -c 'tail -f /log/<ns>/<svc>'   # shell A
WEFT_ENDPOINT=http://localhost:18081 weft sh -c 'tail -f /log/<ns>/<svc>'   # shell B
```
Ingest a row via whichever path lands on either coordinator and confirm
it appears on **both** tail sessions before disrupting anything (this
proves the Redis cross-coordinator hop is live).

**Action — pick one**:
- Restart: `kevcons aws elasticache reboot-cache-cluster --cache-cluster-id weft-redis-001`
- Partition (safer, reversible): revoke the coordinator→Redis
  security-group ingress rule on port 6379, then re-authorize it after
  observing the outage:
  ```
  kevcons aws ec2 revoke-security-group-ingress --group-id <redis-sg-id> --protocol tcp --port 6379 --source-group <coordinator-sg-id>
  # ...observe...
  kevcons aws ec2 authorize-security-group-ingress --group-id <redis-sg-id> --protocol tcp --port 6379 --source-group <coordinator-sg-id>
  ```

**Expected**: `newRedisFanout` never `Ping`s at construction and ingest
never blocks on Redis — the durable path should be entirely unaffected.
`tailfanout_event op=publish err="..."` / `op=psubscribe err="..."` lines
appear on both coordinators during the outage. Once Redis is reachable
again, go-redis's own reconnect logic fires `OnConnect`, logging
`tailfanout_event op=connect` on both.

**PASS if**: `op=connect` reappears on both coordinators after the
disruption ends; both tail sessions resume delivering cross-coordinator
rows without a manual restart; ingest throughput/latency is unaffected
throughout (compare against the tail-fanout latency baseline in
`docs/known-issues.md`: sparse p50/p95/p99 ≈ 817ms/1.536s/1.599s,
burst p50/p95/p99 ≈ 16/26/26ms — those numbers are dominated by an
unrelated ingest-batching ticker, not Redis, so use them as a rough
sanity bound, not a strict pass/fail threshold).

**FAIL signals**: a coordinator process crashes or crash-loops during
the outage; ingest measurably degrades (would mean tail's best-effort
backpressure leaked onto the durable path); no `op=connect` ever refires
after Redis recovers.

**Result (2026-09-19) — PASS, reduced scope.** Prerequisite confirmed:
`weft-redis` (`weft-redis-001`, Valkey 8.1.0) was `available`. Skipped the
pinned-tail-session setup entirely: it depends on `WEFT_ENDPOINT`, which
is undocumented client-side plumbing (now flagged as a TODO in
`shell/config.go` to compile out of release builds — not a public API),
and separately this session's sandbox classified opening the
`AWS-StartPortForwardingSession` tunnels as sensitive remote exec and
blocked it. Relied instead on `redisFanout.publish()`'s unconditional
per-batch call (`coordinator/tail_fanout_redis.go`) — it fires on every
ingest batch regardless of whether any client is tailing, and only logs
`tailfanout_event op=publish err=...` on failure — so ordinary background
ingest traffic (`/log/vimgym/api/heartbeats.log`, ~30s cadence) was
enough to exercise the path with no client of any kind attached.

**Unplanned finding**: the first action attempted — revoking the
coordinator→Redis security-group ingress rule (the plan's "safer,
reversible" option) — produced **zero** `tailfanout_event` lines over 4
minutes. Security groups are stateful and only block *new* connection
attempts; the coordinators' existing pooled Redis connections kept
working because traffic (`publish()` calls) kept flowing on them,
resetting whatever connection tracking exists at the flow level. A
pure SG-based partition does not actually disrupt an already-open,
actively-used connection — only a fresh connection attempt would see it.
Worth remembering for any future network-partition test against a
long-lived pooled connection: revoke-and-wait is not sufficient on its
own.

Switched to the plan's other listed action — `reboot-cache-cluster`
(`weft-redis-001`, node `0001`) — with the ingress rule still revoked, so
the reboot forced existing sockets closed server-side and the SG kept
reconnects from succeeding. Timeline:
- `17:18:10` — ingress revoked (no effect observed through `17:22`, per
  the finding above).
- `17:23:57` — `reboot-cache-cluster` issued.
- `17:24:08` — first `tailfanout_event op=publish err="EOF"` on
  `i-03658aa89a13db588` (the stale pooled connection finally broke).
- `17:25:26` — ingress re-authorized (description restored via
  `modify-security-group-rules` since the shorthand `authorize` form
  drops it — final SG state verified identical to baseline).
- `17:25:25`–`17:25:29` — burst of `op=publish err="dial tcp
  172.31.129.14:6379: i/o timeout"` on **both** coordinators (reconnect
  attempts racing the still-just-restored ingress).
- `17:25:28`–`17:25:43` — `op=connect` fires repeatedly on both
  coordinators (multiple internal connections — publish client and
  PSubscribe client — reconnecting independently); no further errors
  after.
- `/log/vimgym/api/heartbeats.log` shows zero gaps across the entire
  window (`17:23:22` through `17:25:52` at the usual 30s cadence) —
  durable ingest was completely unaffected by the Redis outage, as
  designed.
- Neither coordinator's `MainPID`/`ActiveEnterTimestamp` changed
  (`i-03658aa89a13db588` stayed at `663`/`13:34:28`,
  `i-04fdbfb16e007e33f` at `666`/`16:12:24`) — no crash or crash-loop.

**Net verdict**: every criterion in the PASS list is satisfied except
the one requiring pinned live tail sessions — publish errors appeared
during the outage on both coordinators, ingest never blocked or
degraded, no crash occurred, and `op=connect` refired on both after
recovery. What's *not* independently proven here: that a client actively
tailing via coordinator A actually received a row ingested via
coordinator B (the strongest form of the cross-coordinator fanout
claim). Re-run with pinned sessions (via the port-forward tunnels, once
someone outside this sandbox's remote-exec restriction runs them) if
that specific proof is ever needed.

## Test 5 — shard-fsm-leader flap under load

**Baseline**: `SELECT * FROM leases WHERE name = 'shard-fsm-leader';`

**Action**: take down the current leader (same procedure as Test 1 —
`systemctl stop` + `mask`, since this tests cross-instance failover just
like Test 1, not restart recovery like Test 2) —
`evaluateStateMachines()` runs all shard `Evaluate` calls sequentially in
one loop (not per-shard goroutines), so killing at any moment has a real
chance of landing mid-tick; no special timing needed.

**Expected**:
1. `evaluate_tick`/`evaluate_event` lines stop flowing from the killed
   instance immediately.
2. Deadman's switch: if the *survivor* were the stale one it would
   self-release before renewing (`progressStale`: no progress for >
   2×`PollInterval` = 2 minutes) — not the expected path here, since the
   kill target is the leader itself, not a stalled one. The expected path
   is simple TTL expiry: once `shard-fsm-leader`'s `expires_at` passes,
   the survivor's own `leaderTick` (10s cadence) wins `AcquireLease`.
3. Survivor logs `provisioner: acquired shard-fsm-leader lease` and
   `lease_event ... event=acquired`; `evaluate_tick` lines resume from
   the survivor's `coordinator_id`.
4. `EC2Provisioner.Launch` checks for a live instance already tagged with
   the target `shard_id` before calling `RunInstances`, so a leadership
   handoff mid-launch should not mint a duplicate.

**PASS if**: re-election completes within ~40s; `evaluate_tick` lines
resume without gap beyond that window; no shard ever shows two
concurrent EC2 instances tagged with the same `shard_id`:
```
kevcons aws ec2 describe-instances --filters "Name=tag:shard_id,Values=<id>" "Name=instance-state-name,Values=pending,running"
```
should return exactly one instance, before and after.

**FAIL signals**: no re-election within ~2×`LeaseTTL`; two live instances
for the same `shard_id`; any shard's persisted state showing an
obviously torn write.

## Test 6 — Slow-shard convergence stall

**Purely observational** — no fault-injection knob exists in the code
(checked `logdb/*.go` for a debug slow-down flag; none). This tests
whether the known gap in `docs/plans/coordinator-horizontal-scaling.md`
("`evaluateStateMachines` runs shard `Evaluate` calls in a single
sequential loop... so one shard's slow archive already blocks every
other shard's convergence for that tick") is actually observable under
real load.

**Action**: none — just watch. A shard mid-`ARCHIVING` (export→S3 +
volume-retire, bounded by a 10-minute `archiveTimeout`) is the most
naturally slow `Evaluate` call likely to occur without forcing anything.

**Expected**: if the hypothesis holds, a tick containing a slow shard's
`evaluate_event` should show a `evaluate_tick ... duration_ms=` roughly
equal to the sum of that tick's `evaluate_event` durations — i.e. the
slow shard's time is fully additive to the tick, not overlapped.

**PASS/confirms the gap if**: `evaluate_tick` duration ≈ sum of that
tick's `evaluate_event` durations when a slow shard is present.

**Disproves the gap if**: a tick with one large `evaluate_event`
duration doesn't correspondingly inflate `evaluate_tick`'s total
(shouldn't happen given the code is a plain sequential loop, but that's
exactly what this scenario exists to confirm empirically rather than
trust from reading the code).

Query once enough natural samples have accumulated:
```
weft sh -c 'cat /log/weft/coordinator/main.log | grep evaluate_ | tail -n 500'
```

**Result (2026-09-19) — PASS, confirms the gap.** No shard was actually
mid-`ARCHIVING` during the observation window (checked `main.log` for
`archiv` over the last 7 days — the only archive events were 2026-09-14,
long settled), so this confirms the mechanism via ordinary DuckDB-query
jitter rather than a real 10-minute archive stall. That's sufficient: the
hypothesis is about the sequential loop's structure, not about archiving
specifically. Two natural samples, both from the same coordinator
(`i-0b340ce3672315ed2`) in a 15:50–16:10 window with elevated tick
durations (80–417ms vs. a ~35–90ms baseline):
- `15:57:42`: `evaluate_tick duration_ms=357`, dominated by one shard at
  `duration_ms=71` plus several others at 22–29ms; sum of that tick's 34
  `evaluate_event` lines = 325ms (91% of the tick total, remainder is
  fixed per-tick overhead).
- `16:05:42`: `evaluate_tick duration_ms=417`, dominated by a `kind=bucket`
  event at `duration_ms=96` and one shard at `duration_ms=55`; sum of
  that tick's events = 364ms (87% of the tick total).

Both ticks: elevated event durations correspond directly to an elevated
tick total, with the gap between sum-of-events and tick-total staying in
the same ~30–55ms range regardless of how slow the events were — i.e.
the slow work is additive, not overlapped, exactly as the sequential-loop
code predicts. Logged as a confirmed (not just theoretical) gap in
`docs/known-issues.md`.

## Test 7 — Scale event (`coordinator_desired` 2→3→2)

**Note**: scaling to 3 requires bumping `max_capacity` too —
`infrastructure-catalog/stacks/asg-weft-application/main.tf`'s
`coordinator_asg` module call hardcodes `max_capacity = 2`. A bare
`coordinator_desired = 3` tfvars edit will fail validation without also
raising `max_capacity` for the up-leg. Both are protected IaC applies —
`terraform plan` first, human-run `apply`, per the item-1a precedent.

**Action**:
1. Edit `infrastructure-live/prod/weft/terraform.tfvars`:
   `coordinator_desired = 3`; bump `max_capacity` in
   `infrastructure-catalog/stacks/asg-weft-application/main.tf`'s
   `coordinator_asg` block; `terraform plan` then `apply`.
2. Wait for the 3rd instance to reach `InService` and rejoin (~1-2
   ticks).
3. Revert both edits: `coordinator_desired = 2`, `max_capacity` back
   down; `plan` then `apply`.

**Expected / PASS if**:
- After 2→3: three `InService` coordinators; `nat-holder` and
  `shard-fsm-leader` each show exactly one live (non-expired) holder
  among the three — no split-brain:
  ```sql
  SELECT name, holder, expires_at FROM leases WHERE name IN ('nat-holder','shard-fsm-leader');
  ```
- After 3→2: the terminated instance's `nat-holder`/`shard-fsm-leader`/
  `cron:*` leases (if it held any) get cleanly reclaimed by a survivor
  within one tick — these three run continuous-retry election/reconcile
  loops.

**Expected likely FAIL, called out in advance** *(moot post-cutover — no
`poller:*` leases can exist now that `coordinator/poller` is deleted;
left for historical context)*: any `poller:<id>` lease
the terminated instance held will **not** be reclaimed — `Poller.Start()`
has no reconcile loop (Gotcha B). Check explicitly:
```sql
SELECT l.* FROM leases l WHERE l.holder NOT IN (<current running coordinator instance ids>);
-- or, simpler:
SELECT * FROM leases WHERE expires_at < now();
```
Finding a stuck `poller:<id>` row here is expected, not a test-harness
bug — it's the same gap Test 2 exists to surface, just triggered by scale
churn instead of a direct kill. Log it in `docs/known-issues.md` as a
confirmed gap (a `Poller` reconcile loop mirroring `CronExecutor`'s is
the fix) if found.

**FAIL signals**: two coordinators both driving `evaluateStateMachines`
simultaneously (duplicate `evaluate_tick` lines with overlapping
timestamps from two different `coordinator_id`s) — true split-brain, not
just the expected poller gap.

## Test 8 — Steady-state load check

**Action**: none — observe the existing dogfood `ops` dashboard's
"Multi-Coordinator" section (per-instance memory/disk, coordinator-count
tile, per-instance runtime breakdown, lease-holder table, lease-flap
sparklines — built in Phase 2) over a normal traffic period, comparing
the two `coordinator_id`s against each other.

**PASS if**: no persistent routing imbalance — CPU/goroutines/fds/request
volume stay roughly symmetric between the two instances rather than one
running hot while the other idles.

**FAIL signals**: one coordinator consistently elevated relative to the
other with no corresponding difference in what it's responsible for
(e.g., holding more leases than the other over time — cross-check against
the lease-holder table).

**Caveat**: there is no documented non-tail query-latency baseline from
before the `coordinator_desired` 1→2 rollout (only the tail-fanout
numbers cited under Test 4 exist). A true before/after comparison isn't
retroactively capturable — this test is limited to checking for
imbalance *within* the N=2 steady state, not regression against a
single-coordinator baseline that was never measured.

**Result (2026-09-19) — PASS, with two findings.** Used `runtime.log`/
`metrics.log`/`main.log` directly rather than the dashboard UI (same
underlying data). Over a 2h window (n=240 samples/coordinator,
`i-0681c50b964d1a611` and `i-0b340ce3672315ed2`):

- **goroutines/heap are lopsided but fully explained by lease-holding,
  and CPU stays symmetric.** `i-0b340ce3672315ed2` averaged 192
  goroutines / 65MB heap vs. `i-0681c50b964d1a611`'s 94 goroutines /
  29MB — roughly 2x. Cross-checking `lease_event` lines explains it
  completely: `i-0b340ce3672315ed2` holds **all** ~36 cron leases plus
  `nat-holder` and `shard-fsm-leader` (a full sweep at `20:24`–`20:39`
  today, apparently following a lease transfer — `i-0681c50b964d1a611`
  lost `shard-fsm-leader` at `20:23:17`), while `i-0681c50b964d1a611` has
  held nothing since and is denied every cron lease on every ~30s
  reconcile tick. Despite that, host-level CPU stayed symmetric and
  nearly idle on both (`ip-172-31-12-217` 99.21% idle,
  `ip-172-31-8-67` 99.38% idle) — at current dogfood traffic levels the
  background-work concentration doesn't translate into a visible hot/cold
  split. This matches the test's own PASS bar (imbalance has a
  corresponding difference in responsibility), but it's worth noting the
  concentration is *total*, not partial: one coordinator currently does
  100% of the fleet's singleton background work by design (leases are
  sticky-until-failure, never rebalanced for fairness), which is a
  sharper version of the "consistently elevated" scenario the test
  anticipated than actually finding a 60/40 split would have been.
- **Could not independently verify request-volume symmetry.**
  `queries.log` carries no `coordinator_id` field (unlike
  `heartbeats.log`/`runtime.log`, tagged in Phase 1), so there's no way
  to attribute customer query load to a specific instance from logs
  alone. Filed as a new gap in `docs/known-issues.md` — this is distinct
  from the already-fixed heartbeat/runtime-stats tagging gap.

---

## After running all 8

Update `docs/known-issues.md` with whatever breaks, per Phase 5 of
`docs/plans/coordinator-scaling-live-testing.md`. The poller
no-reconcile-loop gap (Tests 2 and 7) is already expected to land there
regardless of how the rest turn out.
