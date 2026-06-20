# Internal wire versioning (coordinator ↔ shard ↔ compute)

> **Status: stub.** Captures a problem surfaced while landing the
> retention janitor's Archive-RPC change (see
> [`retention-janitor.md`](retention-janitor.md)). Not yet a design —
> this records the gap and the open questions so we can decide whether
> to build anything before the next genuinely-breaking internal change.

## The gap

weft has three internally-versioned binaries that talk over gRPC:
**coordinator**, **shard** (`cmd/shard`), and **compute**
(`compute` package / the gpipe pool role). There is **no runtime
version negotiation or compatibility gate on any of these wires.** A
call either happens to be proto-compatible and works, or it isn't and
fails at the call site. The shard heartbeat carries `binary_sha`, but
that's used only for logging and `weft-admin roll-all`'s skip-if-current
— it is not a compatibility check.

The CI tripwire `scripts/check-wire-bump.sh` only guards the **external**
wires: it bumps `cmd/weft/version.go` (CLI) and `cmd/weftd/version.go`
(agent). The coordinator↔shard and coordinator↔compute protocols have
**no version const and no policing at all** beyond "the AMI builds all
three from one commit."

## Why it bites: staggered deploy = version skew

A `kevcons deploy` does not move the three binaries together:

- **Coordinator** — instance-refreshed first.
- **Compute** (and other non-shard `additional_roles`) —
  instance-refreshed next, automatically, in the same deploy run. Skew
  window: short, bounded by the deploy.
- **Shards** — `shard_roles`, **explicitly skipped** by kevcons
  (FSM-managed, stateful EBS). They only pick up the new binary when an
  operator later runs `weft-admin roll-all --target-sha <sha>`. Skew
  window: **minutes at best, indefinite if roll-all is forgotten or
  stalls mid-fleet.**

So at steady state during/after a deploy you can have a **new
coordinator driving old shards** (the long, possibly-permanent case) and
briefly a **new coordinator driving not-yet-refreshed compute**. The
binding direction is almost always *newer caller → older callee*, and
for shards that skew can persist arbitrarily long.

Today's only defense is discipline: **additive proto changes + callers
that degrade gracefully against an older callee** (e.g. the janitor
treats a missing `customer_stats` as "scan the file"). That worked for
the Archive change because it was purely additive. It will not save us
from a change that renumbers/retypes a field, changes a field's
semantics, or makes the coordinator hard-depend on a new shard RPC —
that breaks for the entire skew window, which for shards may be "until
someone notices."

## Open questions

1. **Do we even need negotiation, or is additive-only discipline
   enough?** Most internal changes can be additive. Maybe the answer is
   a documented rule + a lint, not a protocol.
2. **A declared internal wire version + capability handshake?** e.g.
   shard reports a `wire_version` (or feature flags) on heartbeat; the
   coordinator gates new-behavior calls on it and falls back otherwise.
   Turns "breaks silently" into "degrades explicitly."
3. **A compatibility window contract.** State plainly that a coordinator
   at version N must support shards at N-1 (or N-k), and make
   `check-wire-bump` cover the internal protos with their own version
   const so the obligation is visible at review time.
4. **Force shards current.** Make roll-all part of the deploy (or alarm
   when `shard_records.binary_sha` lags the coordinator's by > X), so
   the skew window can't be indefinite. Doesn't fix breakage, but bounds
   it.
5. **Where does compute sit?** Does compute talk to shards directly, or
   only via the coordinator? That decides whether compute↔shard is a
   third wire to reason about or a non-issue.

## Trigger

Revisit this before the first internal change that is *not* safely
additive. Until then, the rule of record is: **internal RPC changes are
additive, and the newer side degrades gracefully against the older
side** — the same property that made the janitor's Archive change safe.
