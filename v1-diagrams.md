# Shard lifecycle — comprehensive flowcharts

Companion to [`v1.md`](v1.md). Three diagrams covering the launch / live /
wind-down loop:

1. **Shard state machine** — every state a shard row can occupy and
   the transitions between them.
2. **Lifecycle hook decision tree** — what the SQS-driven hook does
   when a new ASG instance hits `Pending:Wait`. This is where the
   orphan-christen ABANDON branch lives.
3. **Reconciler tick** — the order of work each `PollInterval`.

`StateExpiring` is defined in `coordinator/registry.go:22` but no
transition writes it. Treat it as a ghost.

---

## 1. Shard state machine

```mermaid
stateDiagram-v2
    direction TB

    [*] --> LU : reconciler op 2 (under-cap)\nor op 4 (pre-warm)\nmintLaunchingRow → INSERT

    state "LAUNCHING\ninstance_id=''\nshard_id, window,\ntoken, tier set" as LU
    state "LAUNCHING\ninstance_id stamped\nEBS created+tagged+attached" as LC
    state "INGESTING — live\naddr+id populated\nshard.json on EBS" as IL
    state "INGESTING — stale\naddr+id wiped\nLastHeartbeatAt set\n(zombie if rebind never lands)" as IS
    state "READ_ONLY\nno new writes\nstill serves queries" as RO
    state "ARCHIVED\nParquet on S3\nColdShard handle" as AR
    state "ABANDONED instance\n(terminal for instance,\nNOT for any shard)" as ABI

    LU --> LC : Hook CHRISTEN\nClaimLaunchingForTier\nCreateForShard, AttachToInstance\nCONTINUE
    LU --> [*] : reapStaleLaunching\n>10 min, instance_id=''\nDELETE row
    LU --> ABI : Hook ABANDON — surprise instance\n(refresh / out-of-band scale)\nno row to claim → CHRISTEN fails\nrow stays unclaimed

    LC --> IL : christen\nempty-id heartbeat from i-X\nFindLaunchingByInstance\nConfigure RPC\nUPSERT LAUNCHING→INGESTING
    LC --> ABI : reapStaleLaunching\n>10 min, stamped, no heartbeat\nABANDON-signal ASG, DELETE

    IL --> IS : reapDeadShards\nheartbeat stale >2 min\nentry.EBSVolumeID populated\nTerminateForReplacement\n(decrement=false)
    IS --> IL : rebind\nreplacement instance reads\nshard.json, heartbeats with\ndurable shard_id\nupdate addr+InstanceID
    IS --> AR : rebindTimeout (15 min)\nentry presumed lost\n→ archive zombie

    IL --> RO : op 3 RotateToReadOnlyOp\nnow ≥ WindowEnd
    IS --> RO : op 3 (same trigger)

    RO --> AR : op 5 ArchiveShardOp\nnow − WindowEnd > RetentionPeriod\nDuckDB → Parquet\nswap to ColdShard\nsnapshot+detach+delete EBS

    AR --> [*] : op 6 TerminateArchivedInstanceOp\nTerminateInstanceInASG\n(decrement=true)\nnull InstanceID\n(ColdShard keeps serving)

    ABI --> [*] : ASG terminates instance\nno data lost (volume not\ncreated in ABANDON branch)

    note right of LU
      RECOVER (hook) bypasses LAUNCHING entirely:
      orphan EBS in tier+AZ tagged with a
      registry shard_id whose heartbeat is stale
      → reattach, instance heartbeats durable id
      → IS→IL rebind without consuming a row
    end note

    note right of IL
      syncASGDesiredCapacity, every tick:
      desired = Σ bucket.target
              + READ_ONLY count (per tier)
              + future-window LAUNCHING (per tier)
      Drives ASG actual via SetDesiredCapacity.
    end note
```

---

## 2. Lifecycle hook decision tree

Where v1.md's bug lives — the `ABANDON: no LAUNCHING row` path is
hit any time AWS launches an instance the reconciler didn't pre-mint
a row for (instance refresh, manual scale, ASG self-healing during
the 2-min `recognisedAsDead` blackout window).

```mermaid
flowchart TD
    Hook[EC2_INSTANCE_LAUNCHING<br/>SNS → SQS → coordinator<br/>hook fires for new instance i-X]
    Hook --> T{ASG name in tierByASG?}
    T -- no --> A1[ABANDON<br/>unknown ASG]
    T -- yes --> AZ{DescribeInstances<br/>→ AZ?}
    AZ -- err --> A2[ABANDON<br/>AZ lookup failed]
    AZ -- ok --> LV[ListAvailableByTier tier+az<br/>filter to volumes whose<br/>shard_id is recognisedAsDead]
    LV --> N{recoverable count}
    N -- "1" --> R[RECOVER<br/>AttachToInstance volumeID, /dev/sdf<br/>CONTINUE]
    R --> RB[Instance reads shard.json<br/>heartbeats durable shard_id<br/>→ rebind path in ProcessHeartbeat]
    N -- "≥2" --> A3[ABANDON<br/>ambiguous recovery]
    N -- "0" --> CL{ClaimLaunchingForTier<br/>SELECT FOR UPDATE SKIP LOCKED<br/>oldest row, instance_id=''}
    CL -- "false" --> A4[ABANDON<br/>no LAUNCHING row<br/>← refresh / out-of-band scale<br/>← prior shard heartbeat<br/>   still 'live' so RECOVER<br/>   filtered the volume out]
    CL -- "true" --> CV[CreateForShard<br/>gp3 volume tagged<br/>shard_id=claimed.ID]
    CV --> AT{AttachToInstance<br/>/dev/sdf}
    AT -- err --> A5[ABANDON<br/>attach failed]
    AT -- ok --> C[CHRISTEN<br/>CompleteLifecycleAction CONTINUE]
    C --> HB[Instance boots, fresh EBS,<br/>empty shard.json,<br/>heartbeats with shard_id=''<br/>→ christen path promotes<br/>LAUNCHING → INGESTING]

    classDef abandon fill:#fdd,stroke:#900,color:#400
    classDef good fill:#dfd,stroke:#090,color:#040
    class A1,A2,A3,A4,A5 abandon
    class C,R good

    note1["recognisedAsDead:<br/>shard_id in registry,<br/>state ≠ ARCHIVED/EXPIRING,<br/>no heartbeat in last deadFor (2 min).<br/>The 2-min window is exactly<br/>what blocks RECOVER from saving<br/>a refresh-launched instance:<br/>the just-terminated original's<br/>heartbeat is still 'live'."]
    LV -.-> note1
```

---

## 3. One reconciler tick

```mermaid
flowchart TD
    Tick[ticker fires<br/>PollInterval = 5 min prod / 5 s dev]
    Tick --> AH[adoptFromHeartbeats<br/>LiveShardHeartbeats 2 min<br/>→ ProcessHeartbeat each:<br/>christen / rebind / unsuspend / adopt]
    AH --> RD[reapDeadShards<br/>for each registry entry not in heartbeats:<br/>EBS-bound → TerminateForReplacement,<br/>wipe addr+id, await rebind<br/>or 15 min timeout → archive<br/>else → Remove + TerminateInstance]
    RD --> RL[reapStaleLaunching<br/>>10 min old:<br/>id='' → DELETE<br/>id set, no HB → ABANDON+DELETE]
    RL --> Check[check — per-bucket loop]
    Check --> Snap[snapshotForBucket<br/>Entries from registry,<br/>LaunchingCount/FutureLaunching from DB,<br/>IngestingCount derived from registry walk<br/>← orphan-christen fix lives here]
    Snap --> Reconcile[reconcile snap → ordered ops]

    Reconcile --> O1[op 1 AddAppToShardOp<br/>app drift — bucket.Apps not on shard]
    O1 --> O3[op 3 RotateToReadOnlyOp<br/>INGESTING past WindowEnd]
    O3 --> O2[op 2 LaunchShardOp<br/>for i := effective; i&lt;target<br/>where effective = LaunchingCount<br/>+ registry-walked INGESTING<br/>→ provisionForBucket → mintLaunchingRow]
    O2 --> O4[op 4 PrewarmNextWindowOp<br/>WindowEnd − now ≤ PrewarmLead<br/>and FutureLaunching=0<br/>→ Target × mintLaunchingRowForWindow]
    O4 --> O5[op 5 ArchiveShardOp<br/>READ_ONLY past RetentionPeriod<br/>→ Parquet export, ColdShard,<br/>retire EBS volume]
    O5 --> O6[op 6 TerminateArchivedInstanceOp<br/>ARCHIVED w/ instance_id<br/>→ TerminateInstanceInASG<br/>(decrement=true)]

    O6 --> Sync[syncASGDesiredCapacity<br/>per tier:<br/>desired = Σ bucket.target<br/>          + count READ_ONLY<br/>          + count future-window LAUNCHING<br/>clamp to MaxSize, log if clamped<br/>SetDesiredCapacity tier, desired]
    Sync --> Done[next tick]

    classDef bug fill:#ffd,stroke:#a80,color:#640
    class Snap,O2 bug
```

The two yellow-tinted boxes in (3) are where the v1.md story plays
out: `Snap` is the post-fix DB-cache walk (used to inflate
`IngestingCount` with zombie DB rows pre-fix; now correct); `O2` is
what *should* mint a recovery LAUNCHING row when an instance refresh
kills a shard, but doesn't — because `IngestingCount` stays at target
until the 15-min `rebindTimeout` flushes the stale entry to
ARCHIVED.
