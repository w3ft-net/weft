# Plan: Read-time field redaction

Per-namespace rules that mask specific JSON fields based on the reading user's role. Distinct from ingest-time redaction (`docs/plans/compliance/ingest-governance.md`): ingest-time redaction means the value never lands on disk; field-redaction means the value persists but is masked at query time when a less-privileged role reads it.

## Why both, not one

The two are not redundant — they answer different questions:

- **Ingest-time redaction** (governance plan): "credit-card-shaped substrings should never be persisted." Applies uniformly; once redacted, the original is gone.
- **Read-time redaction** (this plan): "engineers can see the customer email; support reps cannot." The original persists; access is gated.

A serious customer typically wants both. Ingest-time for things they're confident they should never store (PII the application accidentally logs). Read-time for things they need to keep (customer ID, email) but want to limit visibility on.

## Why now-ish, not later

This is a feature serious customers (regulated industries, anyone with a SOC 2 program) ask about during procurement. Without it, the answer is "everyone on your team can see every field your apps log" — accurate but a hard sell to a buyer with internal RBAC requirements. The implementation is small enough that we should not put it off behind a customer asking, since the asking happens during a sales conversation we can lose.

## Architecture

Per-namespace redaction rules live alongside the existing `NamespacePermission` model. A rule:

```yaml
# stored on the namespace record; edited via UI or `vi /etc/redaction/<namespace>.conf`

rules:
  - field: ".user.email"
    mask_for_roles: [support, viewer]
    mask: "<redacted>"

  - field: ".session.token"
    mask_for_roles: ["*"]                 # mask for everyone except namespace owners
    mask: "***"

  - field: ".payment.card_number"
    mask_for_roles: ["*"]
    mask_strategy: last4                  # show only last 4 digits
```

Evaluation happens in the coordinator's response path:

```
coordinator.Query(...)
  → fan out to shards, get LogRows back
  → for each row:
      walk payload JSON; for each redaction rule that fires
        for the caller's role + this namespace:
          replace value with mask
  → stream to client
```

Roles come from the existing `NamespacePermission` model. A user has a set of roles per namespace; a rule fires if `caller_roles ∩ rule.mask_for_roles ≠ ∅`. The wildcard `"*"` means "every role except owner" — owners always see unmasked data so somebody can debug the rule itself.

## Mask strategies

Three out of the box, plus an extension point:

- **`<literal>`** — replace with a fixed string (default: `"<redacted>"`).
- **`last4`** — keep last 4 chars, mask the rest. Useful for card numbers, tokens.
- **`hash`** — replace with `sha256(value)[:8]`. Lets a support rep verify "is this the same user?" without seeing the user.

Only literal in v1; the others are scoped for v1.5 once we see what customers actually want.

## Performance

Read-time redaction adds per-row work proportional to (rule count × field count). Both numbers are small in practice — typical customers will have 5-20 rules, payloads have <100 keys. JSON walking with a pre-compiled rule path index is microseconds per row.

The bigger concern is that redaction defeats the planner's pushdown for predicates against masked fields. If a query says `jq 'select(.user.email == "foo")'` and `.user.email` is masked for the caller, the predicate would never match — but at the planner level we don't know the caller's role yet, so the WHERE pushes down and matches at the shard. Then redaction happens at the coordinator post-result.

Net effect: a less-privileged user can still *use* a masked field as a query filter and get correct rows back, just with the field masked in the result. That's actually the desired semantic — the field is gated for *display*, not gated as a query handle. If we wanted to gate it as a query handle, we'd refuse the query entirely. We don't.

There's a side channel here: a caller can confirm specific values by issuing predicate queries, even when they can't see the field. This is the standard "redaction is not access control" tradeoff. For the strong-control case the customer needs ingest-time redaction or namespace-level permission denial, not field redaction.

## Components to build

1. **`store.NamespaceRedactionRule`** — list per namespace: `{field_path, mask_for_roles[], mask_literal}`.
2. **`coordinator/redaction.go`** — rule compilation (parse field paths once), per-row evaluation (walk JSON, apply masks). Caller-role lookup memoized per request.
3. **Hook in the coordinator query response path** — `coordinator/coordinator.go`'s row-streaming handler runs each row through the redactor before yielding to the client. Watch evaluation runs *unredacted* — watches are server-side and need full data; redaction is purely a client-facing transform.
4. **Shell editor** — `vi /etc/redaction/<namespace>.conf` opens the rules YAML for that namespace; save-time validation rejects bad field paths and unknown roles.
5. **Web UI** — Namespace settings → Redaction tab. Form-based rule list + a test pane: paste a sample log row, see what each role would see.
6. **Audit log integration** — rule changes emit events (`redaction.rule_save`, `redaction.rule_delete`) into `docs/plans/compliance/audit-log.md`.
7. **Docs** — `docs/manual/field-redaction.md` covering the rule grammar, side-channel caveat, the "mask for everyone except owners" wildcard, and the three plan combos: ingest-time only, read-time only, both.

## Subtle interaction with watches and `/tmp`

Watches run server-side under a customer-scoped identity, not a per-user role. They see unredacted data. A watch that emits a redacted-field value through `print` and then a less-privileged user reads `weft.latest(watch)` would leak the value.

Two ways to handle:

- (a) Run watches under a designated "redaction-aware" identity that respects rules.
- (b) Document the interaction: "watch results are unredacted; redact in the watch body if you need to expose to lower roles."

(b) is simpler and matches "watches are explicit, redaction is for raw query display." Ship with (b); revisit if customers ask.

`/tmp` outputs are similarly unredacted — written by a user who could see the raw data. A second user reading `/tmp/foo` sees what was written, redaction-free. Documented.

## Estimated effort

- `NamespaceRedactionRule` model + CRUD: **0.5 day**
- Compiler + evaluator: **0.75 day**
- Coordinator response-path hook + tests: **0.75 day**
- Shell editor integration + validation: **0.5 day**
- Web UI tab + test pane: **1 day**
- Audit log integration: **0.25 day**
- Docs + side-channel writeup: **0.5 day**
- Tests (per-role visibility, per-strategy masking, watch+`/tmp` boundary): **0.75 day**

**Total: ~5 days**.

## Non-goals

- **No per-user redaction.** Redaction is per-role. Per-user is unbounded ACL territory; if a customer wants per-user, they create more roles.
- **No row-level redaction.** Field-level only. "Hide rows whose `severity == low` from the support role" is a query filter, not a redaction primitive. Use namespace permissions instead.
- **No advanced masking (regex replace, pattern preserving).** Three strategies max in v1.5; literal-only in v1.
- **No client-side redaction.** All masking happens server-side before the bytes leave the coordinator. We don't ship a client that "could" reveal masked values.
- **No retroactive unmask.** Once a rule is in place, cached/exported data outside our control may have already been seen unmasked. We don't try to undo that.
- **No interaction with row-level security (`customer = ...`).** That boundary is hard, immutable, and unaffected by these rules.
