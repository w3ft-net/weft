# Compute scaling (future work)

Placeholder. Captures future work on horizontal scaling for the compute
tier. Not a day-1 concern — today the compute ASG runs
`compute_desired = 1` and the coordinator discovers that single
instance at boot. Recorded here so direct-to-compute (see
`direct-to-compute.md`) can point to it without dragging these
questions into scope.

## Current state (as of ops 1–6 shipping)

- **One compute instance.** `prod/weft/terraform.tfvars` pins
  `compute_desired = 1`. The compute ASG can scale up to
  `compute_max = 10` but nothing currently drives scale-out.
- **Single target.**
  The coordinator takes a single `--compute` address and caches one
  `compute.Client` for the process lifetime. If the compute instance
  is replaced (autoscaling event, AMI refresh, instance failure) the
  coordinator holds a stale private IP until it restarts.
- **No in-process fallback.** The coordinator refuses to start
  without `--compute` (`log.Fatal("no compute addr specified")`);
  there is no path that runs queries inside the coordinator process.
- **SG paths already in place.** `compute_from_coordinator` and
  `compute_from_shard` security-group rules are both wired
  (`stacks/asg-weft-application/main.tf:628,650`), so any future
  re-discovery change doesn't need infra work first.
- **Cost posture.** The stack already runs one ALB (API) and two NLBs
  (coordinator public + coordinator internal). Adding a third
  load-balanced target (compute) for a single-instance tier is not
  justified at current volume.

## Triggers for revisiting

- Compute CPU saturates under normal query load (watch
  `ec2/cpu_utilization` per role; coordinator-side query latency
  starts climbing while shard fetch times stay flat).
- A `compute_desired > 1` fleet is needed for query concurrency —
  once that happens the "take `addrs[0]`" shortcut is actively wrong
  and a real pool / LB is required.
- An incident where a compute instance replacement silently broke
  queries because the coordinator kept its stale address. That
  symptom is very obvious in the logs (gRPC `Unavailable` on every
  compute call) and cheap to fix short-term by bouncing the
  coordinator.

## Rough shape when we need it

Two paths, not mutually exclusive:

### A. Re-resolve in the coordinator

Keep compute pooled by IP list; refresh it periodically.

- Add a goroutine in `cmd/coordinator/main.go` that calls
  `DiscoverAddrs` every ~60s and swaps the active pool atomically.
- Make `compute.Client` accept multiple addrs and round-robin queries.
- Dial retries handle transient replacement; a 60s re-discovery
  catches the address change.
- No infra change. Cheapest. Good enough for <10 compute instances.

### B. NLB in front of compute

Put compute behind an internal NLB and point the coordinator at a
stable DNS name.

- New `aws_lb`-internal + target group attaching the compute ASG on
  port 9010, same pattern as `aws_lb.coordinator_internal`.
- Config blob emits `COMPUTE_NLB=<dns>:9010`.
- `cmd/coordinator/main.go` grows a `--compute-nlb` flag; systemd
  unit switches from `--compute-asg` to `--compute-nlb`.
- gRPC's own connection management handles target churn behind the
  NLB — no re-discovery loop needed.
- Costs one NLB (roughly $20/mo fixed + $0.006/LCU-hour). Worth it
  once compute genuinely runs a fleet.

Picking between them: do (A) first (it's code-only and doesn't add
billing). Flip to (B) if we ever end up with a real compute pool and
the re-discovery goroutine becomes a bottleneck or bug source.

## Related

- `direct-to-compute.md` §6 / "Beyond this plan" — the eventual
  shard→compute direct RPC flow. That will exercise the
  `compute_from_shard` SG rule and compounds the cost of compute
  going away (shards can't route themselves). Re-discovery or NLB
  matters more at that point.
- `az-resilience.md` — compute is stateless so it's easier than
  shards; multi-AZ compute is really just "set ASG subnets across
  AZs" once we have >1 instance.
