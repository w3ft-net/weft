# Ingestion: TLS syslog (platform log streams)

## Overview

Some hosting platforms (Render, and by pattern any future PaaS with the
same shape — Fly.io, Railway, …) only offer an off-platform log path over
**syslog on TLS/TCP**, not HTTP. `SyslogServer` (`api/syslog_server.go`)
is a generic TLS-terminated-upstream syslog listener for exactly this
case: one instance per platform, each bound to its own port, each
constructed with an `ingest/syslog.Producer` that knows how to turn that
platform's syslog fields into weft's `(service, kind, instance)` triple.
Render is the first (and, as of this writing, only) registered producer.

This is a sibling ingest path to CloudWatch (`ingestion-cloudwatch.md`)
and weftd (`ingestion-weftd.md`): all three eventually call the same
coordinator `IngestStream` RPC and land in the same ring buffer / shard
routing / usage metering.

## Architecture

```
Platform (Render, ...)  ──TLS 6514──►  weft-nlb  <producer>.syslog.w3ft.net:6514
                                            │  TLS listener, ACM cert for
                                            │  this hostname, terminates
                                            │  TLS → plaintext TCP to a
                                            │  producer-dedicated port
                                            ▼
                        api instance :<port>  (SyslogServer, built with one Producer)
                          - the port IS the producer identity — no
                            content-sniffing needed
                          - strips the platform's line-prefix auth shape
                          - store.ValidateAgentTokenForProducer(token, name)
                          - ingest/syslog.ParseStream → Producer.MapFields → []*pb.LogRow
                                            │
                                            ▼
                        coordClient.IngestStream()   ← same path weftd + Heroku take
```

`SyslogServer` itself is fully generic — TLS termination happens upstream
at the NLB, so it only ever sees plaintext TCP; framing and RFC 5424
parsing live in `ingest/syslog` (shared with the Heroku HTTPS drain
adapter, `api.HerokuAdapter`, which is the first consumer of that
package). The *only* platform-specific code is one `Producer`
implementation per platform.

## Producer identity comes from the listener, never from content

Each registered `Producer` gets its own NLB TLS listener, own ACM cert,
own hostname, own backend port — not one shared listener disambiguated by
peeking at the token or the frame. This holds even with only one producer
registered today. The alternative — one listener guessing both the auth
shape and the field-mapping rules from content before anything is
validated — was tried in early design and rejected: PPv2 doesn't carry
SNI through AWS's NLB (confirmed with a live packet capture against
Render), so there's no reliable signal to disambiguate on before the TLS
session terminates. Binding identity to the listener also means
`store.ValidateAgentTokenForProducer` can reject a token minted for a
different producer outright, even if it leaked — a Render token can never
be replayed against a hypothetical Fly.io listener.

Cardinality here is hosting *platforms* (a handful, growing slowly), not
tenants, so this doesn't run into an NLB's per-listener limits the way a
dedicated listener per *customer* would.

## Wire-capture first, always

Do not trust a platform's own documentation for its syslog wire shape.
Render's docs implied RFC 5425 octet-counting framing and said nothing
about how a "Token" configured in their UI reaches the stream; a real
wire capture (a throwaway TLS listener plus a disposable Render service)
showed the opposite on both counts: framing is plain LF-delimited
(`ingest/syslog.NonTransparent`), and the token arrives as a **literal
cleartext prefix** (`"TOKEN SP"`) repeated on every line — not in
`STRUCTURED-DATA`, and not consistent with the auth shape Render uses for
its *named* provider integrations (Datadog, Better Stack, …), which each
get their own bespoke format. There is no substitute for capturing real
traffic before writing a `Producer` or an auth path for a new platform.
`ingest/syslog.Frame.StructuredData` exists for a platform that *does* use
SD-based auth (Better-Stack-style) — nothing in `SyslogServer` assumes
Render's line-prefix shape is universal, but nothing generic implements
SD-based auth yet either; that's new code the first platform needing it
will add (see *Adding a new syslog-speaking platform* below).

## Auth model

One token per namespace+env, scoped to the producer it was minted for
(`AgentToken.Producer`, `store.ValidateAgentTokenForProducer`). Unscoped
tokens (`Producer == ""`) still validate against the producer-agnostic
`ValidateAgentToken` used by weftd and Heroku, but never against a
producer-scoped check — so a plain namespace token can't be used to open
a syslog listener even by accident.

`SyslogServer.handleConn` reads whatever the platform's line-prefix or
SD-element format is, resolves the token, then opens a normal
`IngestStream` to the coordinator with `authorization: Bearer <token>` in
gRPC metadata — the coordinator's own `authenticateStream` re-validates
independently, so this listener's check is an additional gate, not a
replacement for the one every other ingest path goes through.

## Rate limiting

Ingest is capped per `AgentToken` (not per connection) via
`config.Runtime.IngestTokenBytesPerSec` / `IngestTokenBurstBytes`, enforced
in the coordinator's `IngestStream` handler (`coordinator/service/ingest_limiter.go`)
for every producer, weftd included — nothing producer-specific here. A
token over budget is throttled (the offending row is delayed until the
bucket refills), not disconnected; the bucket persists across reconnects
so cycling connections doesn't reset it. See that file's doc comment for
why this is deliberately separate from `IngestStreamBudgetBytes` (a
per-connection memory-safety cap that predates it).

## Current producers

| Producer | Port | Hostname                    | Wire shape                                            |
|---|---|---|---|
| `render` | 9023 | `render.syslog.w3ft.net:6514` | LF-framed RFC 5424; token as a literal `"TOKEN SP"` prefix repeated on every line |

## Adding a new syslog-speaking platform

Repeat the recipe below — new hostname, cert, port, `Producer` — rather
than teaching the existing listener to guess. Do the wire capture
(above) before writing any code; every detail below is confirmed for
Render specifically and is not safe to assume for the next platform.

### Code

1. **Capture real traffic first.** Stand up a throwaway TLS listener
   (a Let's Encrypt cert on a disposable hostname works fine) and point
   the platform's log-stream feature at it. Confirm: framing
   (octet-counted vs. LF-delimited — `ingest/syslog.Framing` supports
   both, plus `Detect`), where the auth credential travels (line prefix?
   `STRUCTURED-DATA`? somewhere else entirely?), and which RFC 5424
   header fields carry the platform's own service/process/instance
   identifiers (Render uses `HOSTNAME` for service and
   `APP-NAME`/`MSGID` for kind+instance; another platform may use
   different fields).
2. **Implement `ingest/syslog.Producer`** in a new
   `ingest/syslog/producer_<name>.go`: `Name() string` (the token-scoping
   key and the `--for` value) and `MapFields(Frame) (service, kind,
   instance string, ok bool)`. Return `ok=false` to drop a frame without
   forwarding it (Render does this for its own connectivity-test ping).
   See `producer_render.go` for the shape.
3. **If the auth shape isn't a "TOKEN SP" line prefix**, `SyslogServer.handleConn`
   needs a second code path — today it only implements Render's shape.
   An SD-element credential (`f.SD(id, key)` on the parsed `Frame`) is
   the most likely alternative; there's no framework for it yet, so this
   is new code, not a config toggle.
4. **Register the producer** in `api/handlers.go`'s `syslogProducers`
   slice. This one list drives three things: `weft token create --for
   <name>` validation, the `GET /api/syslog-producers` registry the
   dashboard's token-create form reads to populate its **For** selector,
   and `(*Server).syslogEndpoint`'s `<name>.syslog.<host>:6514` string.
5. **Construct a new `SyslogServer`** in `cmd/api/main.go` next to the
   existing one: `apiSrv.NewSyslogServer(yourProducer, yourPort)`, with a
   new `--<name>-syslog-port` flag mirroring `--render-syslog-port`.
6. **Tests**: a `producer_<name>_test.go` pinning the field mapping
   (mirroring `producer_render_test.go`), plus whatever
   `api/syslog_server_test.go`-style auth-path coverage a new auth shape
   needs. `TestListSyslogProducers` and `TestEndpointsRequireAuth` in
   `api/token_producer_test.go` / `api/auth_required_test.go` extend
   automatically once the producer is registered — no test-side
   registry to update by hand.

### Infra

Reusable per-producer template (`infrastructure-catalog/stacks/asg-weft-application/main.tf`),
applied once for Render (2026-09-02), live in prod:

1. `aws_lb_target_group` — protocol `TCP`, the new producer's port,
   target = the coordinator ASG (already carries gRPC and the api
   server; a new producer doesn't need a new instance or process, just
   one more listener inside the same binary).
2. `aws_lb_listener` on `weft-nlb` — a new port (Render's is `6514`, the
   RFC 5425 standard; a different platform may expect a different one),
   protocol `TLS`, forwarding to the new target group.
3. **ACM cert scoped to the specific hostname** (`<name>.syslog.w3ft.net`),
   not a wildcard. Render's cert is a wildcard (`*.syslog.w3ft.net`) —
   a leftover from briefly considering a shared-listener design
   mid-build — but that was a mistake to repeat: a dedicated cert per
   producer is the intended shape going forward.
4. Route 53: `<name>.syslog.w3ft.net` → `weft-nlb`.
5. Instance security group: allow the new port from the NLB subnet
   (mirrors the existing 9023 rule).
6. Append the new target group's ARN to
   `module.coordinator_asg.target_group_arns`. This ASG carries an
   `instance_refresh` block (`min_healthy_percentage = 100`), so
   applying this triggers a real, zero-downtime rolling replacement of
   the coordinator instance — expected, not a regression, but worth
   knowing before you apply.

> ⚠️ `weft-nlb` is **shared with gravitty** (their `:443` and `:2222`
> listeners live in a separate Terraform state). weft owns the LB and
> its own listeners, so adding a weft listener is in-bounds, but a
> botched apply on this LB takes gravitty down too. See the header
> comment in `main.tf`.

No coordinator or shard changes are needed for a new producer — every
producer converges on the same `IngestStream` RPC, ring buffer, shard
routing, usage metering, and rate limiting that weftd and Heroku already
use.
