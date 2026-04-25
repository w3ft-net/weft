# Plan: Customer-visible audit log

User-facing record of configuration changes — source added, token revoked, watch saved, namespace permission granted, rule edited, billing plan changed. Surfaced in the shell as `/log/_audit.log` so all the existing query, alert, and watch primitives work on it.

## Why add this

Today there's no way for a customer to answer "who revoked my agent token last Tuesday?" or "when was the rule that dropped our DEBUG logs added?" The actions happen; nothing persists a user-visible trail. Three distinct cases this matters for:

1. **Incident postmortems.** "Our metrics went silent at 3am" → "someone revoked the weftd token at 2:57am" is a one-query answer if the audit log exists. Today it's a store guess.
2. **Compliance buyers.** Any SOC 2 Type II customer will ask for an audit trail. Having one is cheap; not having one is a buying blocker later.
3. **Self-service forensics.** "I don't remember adding a watch called `debug-test`" — the customer can find the actor themselves instead of emailing support.

## Architecture

An audit event is a structured fact about a config change. Events are written at every mutation site and surfaced as a log source called `_audit` inside a reserved namespace.

### Event shape

```go
type AuditEvent struct {
    CustomerID uint
    ActorID    string    // user id, or "system" for automated events
    ActorKind  string    // "user", "token", "system"
    Kind       string    // "source.add", "token.revoke", ...
    TargetType string    // "source", "token", "watch", "permission", ...
    TargetID   string
    Summary    string    // human-readable one-liner
    Detail     JSONB     // before/after diff or arbitrary structured context
    CreatedAt  time.Time
}
```

Event kinds are dot-scoped strings (`source.add`, `source.remove`, `token.create`, `token.revoke`, `watch.save`, `watch.delete`, `function.save`, `namespace.create`, `permission.grant`, `permission.revoke`, `ingestrule.save`, `ingestrule.delete`, `silence.add`, `silence.cancel`, `billing.plan_change`, `oidc.config_save`).

### Surfacing as a log source

Events are written to a reserved pseudofile — the coordinator writes every `AuditEvent` both to Postgres (durable, indexed) and into the normal log ingest path as a JSON row under app `_audit` in a customer-reserved namespace. Users query it with every tool they already know:

```
cat /log/_audit.log | grep token
cat /log/_audit.log | jq 'select(.kind == "permission.grant")'
tail -f /log/_audit.log                      # live view during an incident
cat /log/_audit.log --since 7d | jq '.actor_id' | sort | uniq -c
```

Watches and alerts work the same way:

```
# /cron.d/audit-alarming-grants
# schedule: every 5m
cat /log/_audit.log --since 5m \
  | jq 'select(.kind == "permission.grant" and .detail.role == "owner")' \
  | wc -l \
  | alert above 0 --notify=security
```

That last example — "page security when anyone gets promoted to owner" — is a feature we otherwise couldn't offer without building a dedicated alert subsystem for config events. The audit-as-logs approach gets it for free.

### Write path

Every mutation call site (largely in `api/handlers.go`, `api/integrations.go`, `store/shell.go`) gets wrapped:

```go
// before
s.store.SaveWatch(w)

// after
s.audit.Emit(ctx, AuditEvent{
    Kind: "watch.save", TargetType: "watch", TargetID: w.Name,
    Summary: fmt.Sprintf("saved watch %q", w.Name),
    Detail:  map[string]any{"schedule": w.Schedule, "pipeline": w.Pipeline},
})
if err := s.store.SaveWatch(w); err != nil { ... }
```

`audit.Emit` is synchronous into Postgres (cheap, indexed) and asynchronous into the log ingest path (same RingBuffer that weftd lands in). A failure to log doesn't fail the underlying mutation — the Postgres row is the durable copy.

## Components to build

1. **`store.AuditEvent`** — model + `CreateAuditEvent(...)` + `ListAuditEvents(customerID, range, filters)`. Indexed on `(customer_id, created_at)` and `(customer_id, kind)`.
2. **`api/audit.go`** — shared emitter: accepts a minimal event, fills in `ActorID`/`ActorKind` from request context, writes to Postgres, fans out to ingest.
3. **Call-site wrapping** — every mutating handler in `api/`. Roughly 20-30 sites; each gets one `Emit` call. Centralizing the emitter keeps the call sites uniform.
4. **Reserved namespace + pseudofile** — `shellfs/` routes `/log/_audit.log` to a customer-scoped audit feed. No namespace-token write access to this feed; only the coordinator writes.
5. **Ingest-path integration** — audit events go into the existing ingest drain with `customer = <customer>, app = "_audit"`, schema `format: json`. They sit in shards like any other log; they get the same retention the customer's plan provides.
6. **Web UI** — Account → Activity tab. A simple reverse-chronological list with a search box, deep-linking into shell queries for anything beyond "what happened in the last week." The web UI is a fallback for customers not comfortable in the shell; power users will stay in `/log/_audit.log`.
7. **Docs** — `docs/manual/audit.md` with the event-kind vocabulary, example queries, the "alert on risky actions" recipe, and a retention note.

## Retention and scale

Audit events ride the same per-plan retention as regular logs. For small customers this is nothing (maybe a few thousand events over 30 days). For large customers it's still negligible — config mutations are measured in thousands per month, not billions. No need for a dedicated partitioning strategy.

If retention on regular logs is shorter than a customer wants for audit purposes, they can set up a watch that exports `_audit.log` rows to `/tmp` (48-hour scratch) or relies on the Postgres `audit_events` table directly via the web UI, which we could give a longer default retention than `cron_results` if needed (but deferring until asked).

## Security considerations

- **Tamper resistance.** The Postgres copy is authoritative; the ingest-path copy is a convenience. We don't need Merkle chains or sig-per-row — threat model is "operator forensics," not "proving compliance to a hostile regulator."
- **Access control.** Every team member can read their customer's audit log. No per-user scoping; matches team-wide /tmp visibility from `docs/manual/tmp.md`.
- **PII in Detail.** `Detail` is free-form JSON. For kinds that might capture customer data (pipeline text in `watch.save`, token hash in `token.create`), we explicitly list allowed fields per kind so "oh, the full query leaked into audit" doesn't happen.

## Estimated effort

- `AuditEvent` model + Postgres schema + index: **0.5 day**
- Shared emitter + actor resolution: **0.5 day**
- Wrapping ~25 call sites: **1 day**
- Ingest-path wiring + reserved pseudofile: **0.5 day**
- Web UI activity tab: **1 day**
- Docs + example recipes: **0.5 day**
- Tests (one per event kind, check writes land in both stores): **0.75 day**

**Total: ~4.75 days**.

## Non-goals

- **No auditing of reads.** Only config mutations and access-control changes get events. Every query on every log would dwarf the real audit signal — and it's what the logs themselves are for.
- **No immutability / write-once storage.** Postgres row + log-path copy is "good enough" for the operator-forensics threat model. Customers who need cryptographic chain-of-custody are building on the wrong tool.
- **No external SIEM integration in v1.** Since audit events are log rows, the existing notify / export plans (webhook destinations, S3 export) naturally cover "send audit events somewhere else" without new surface.
- **No per-action approval workflow.** "Require two owners to revoke a token" is an enterprise feature; out of scope until a customer names it.
- **No pre-launch audit.** Coordinator-side automated actions (shard lifecycle, archive) do *not* emit audit events; those belong in operator logs, not customer-facing ones. Only human-driven config mutations get recorded.
