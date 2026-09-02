# Plan: Render.com log ingestion

A narrow adapter for customers hosted on [Render](https://render.com). Render's
only off-platform log path is **log streams** — RFC5424 syslog over TLS/TCP.
It is not HTTP, so it does not fit the `Adapter` interface in
[`http-ingest.md`](http-ingest.md); it needs a long-lived TLS syslog listener.
This plan adds that listener **without a new load balancer** (a listener on the
existing `weft-nlb`), plus a zero-new-infra fallback for security-sensitive
customers.

## Status

🟢 Unblocked. Triggered by a live prospect on Render. **Component 1
(`ingest/syslog/`) has shipped** — see *Shared components* below. The
**wire capture is done** (against a throwaway `web` service, `cron` job, and
their build logs) — see *Wire capture results* below. It resolved auth in
favor of neither A1 nor A2: Render prepends the token as a **literal
cleartext prefix** on every line, which is simpler than either. Framing is
**always LF-delimited**, never octet-counted, contrary to the RFC5425
assumption below. The listener is designed as a **generic TLS syslog
server**, not a Render-specific one — Render is the first registered
`ingest/syslog.Producer`, differentiated by field-mapping rules **and by
which NLB listener/port it owns** (a connection's producer is never guessed
from its content — see *Approach A*). A second syslog-speaking platform
(Fly.io, Railway, …) is a new `Producer` implementation plus a new
listener/cert/port for it, not new server code — confirmed necessary, not
just preferred, by a real PPv2 test against the live NLB (AWS doesn't
forward SNI; see *Wire capture results*). See *Shared components*.
**Infra (Component 5) is live in prod and healthy**: `*.syslog.w3ft.net` TLS
listener on `weft-nlb` → `weft-syslog` target group → coordinator :9023, no
new instance (colocated with gRPC and the api server on the same coordinator
ASG). **Components 1-6 have all shipped**, CLI and dashboard both: the
`Producer` interface + `RenderProducer`, the `AgentToken.Producer` schema
change, `SyslogServer` itself (wired into `cmd/api/main.go`, listening on
9023), `weft token create --for render`, the web dashboard's Namespaces
page producer selector + endpoint display, and a per-token bytes/sec rate
limit on `IngestStream` — see *Components to build* below for what landed
in each. **The end-to-end path is live and mintable today**, from either
the CLI or the browser: a real Render customer could be onboarded right
now by running `weft token create --for render` (or minting a
`render`-scoped token from the dashboard), pasting the token + printed
endpoint into Render's log-stream config, and rows would flow all the way
to storage — nothing blocks that. Only Component 7 (docs) remains.

## Why

A Render-hosted customer cannot use either existing ingest path:

- **weftd** tails files; Render services log to stdout/stderr and you cannot
  sidecar a second process into a Render service's container.
- **CloudWatch** is AWS-only.

Render *does* have a first-class "stream logs to a third-party" feature. We
should meet it where it is rather than tell the prospect to re-architect.

## What Render actually gives us

Confirmed from [Render's docs](https://render.com/docs/log-streams) and from how
third-party vendors document receiving Render's stream:

### Transport

- **Syslog over TLS/TCP only** for a generic endpoint. Render's HTTPS/JSON
  delivery is hard-locked to Datadog and Loggly by endpoint pattern-match:
  *"If an endpoint doesn't match a supported HTTPS provider, Render treats it as
  a syslog destination."* We cannot stand up a generic HTTPS receiver.
- Port **6514**, the RFC5425 (`syslog-over-TLS`) standard port — every Render
  provider example (`*.betterstackdata.com:6514`, `newrelic.syslog.*:6514`,
  `syslog.*.coralogix.com:6514`, SolarWinds `:6514`) uses it.
- RFC5425 implies **octet-counting framing** (`MSG-LEN SP SYSLOG-MSG`), a
  64 KB max message, and a **CA-validated server cert** (self-signed is
  rejected). [Axiom's secure-syslog page](https://axiom.co/docs/send-data/secure-syslog)
  states the same limits for Render input.
- Connection is long-lived; Render drops it on an oversized or malformed
  message.

### Frame contents

Render's own example line:

```
<0>1 2021-03-31T16:00:00-08:00 test-service cron-12345 74440 cron-12345 - hello this is a test
```

**Confirmed by the wire capture** (see below) — `HOSTNAME` = the service slug;
`APP-NAME` and `MSGID` are both `{kind}-{instance-id}` (split on the *last*
`-`, since instance ids never contain one — `web-9nk6q`, `cron-stqq5`).
`PROCID` is always the literal `1`, not useful. Kinds seen: `web`, `cron`, and
— undocumented anywhere — `bld` for **build-step output**, delivered on the
same stream. Real `<PRI>` values arrive (not the doc's `<0>` placeholder);
severity reflects a `level` field only when the app emits `logfmt` or JSON, as
documented. Datastore logs (Render Postgres, Key Value) are presumably
included the same way, not yet captured — no datastore in the throwaway test.

### Auth — resolved by the capture

Render carries the "Token" field into the stream **per named provider**, in that
provider's own shape: Better Stack gets `[logtail@11993 source_token="…"]` in
STRUCTURED-DATA, Coralogix `[coralogix@… private_key="…"]`, New Relic prepends
the raw key *before* `<PRI>`. There is no universal format and **no provider
selector** — just endpoint-hostname matching.

For a **generic/unrecognized endpoint**, the capture shows Render falls back
to the New Relic shape, not the Better-Stack/Coralogix one: the token is
prepended as a **literal cleartext string, space-separated, before every
line** —

```
xpoopx <6>1 2026-09-02T14:08:25.185717Z render-capture-testsvc web-9nk6q 1 web-9nk6q - heartbeat #163 hit a deliberate exception
```

— present on *every* frame for the life of the connection, not just once.
`STRUCTURED-DATA` is always `-`; with no token configured the same frame
arrives with no prefix at all. This is neither of the two shapes anticipated
below (A1's SD token, A2's per-tenant hostname) — it needs neither a wildcard
cert nor PPv2/SNI parsing. `SyslogServer` just splits the leading
`TOKEN SP` off each line before framing/parsing and looks the token up
directly. Still a bearer credential (same trust model as an S3 presigned URL
or the Heroku drain URL weft already ships,
`drain.w3ft.net/api/ingest/heroku/wft_<token>`) — but it travels inside the
TLS session instead of in the SNI/DNS label, so it isn't visible to on-path
observers or resolver logs the way A2's hostname-credential model would have
been.

## Approach A — direct TLS syslog listener (recommended)

```
Render service  ──TLS 6514──►  weft-nlb  render.syslog.w3ft.net:6514
                                      │  (TLS listener, ACM cert for this
                                      │   hostname — ACM renews) terminates
                                      │   TLS → plaintext TCP to :9023
                                      ▼
                        api instance :9023  (SyslogServer, built with RenderProducer)
                          - port 9023 IS the producer identity — no
                            content-sniffing needed to know it's Render
                          - strips leading "TOKEN SP" off each line
                          - store.ValidateAgentToken(token, producer="render")
                          - LF-delimited RFC5424 frames → RenderProducer.MapFields → []*pb.LogRow
                                      │
                                      ▼
                        coordClient.IngestStream()   ← same path weftd + Heroku take
```

**Producer identity comes from the listener, never from content.** Each
registered `Producer` gets its **own NLB TLS listener, own ACM cert, own
hostname, own backend port** — not one shared listener disambiguated by
peeking at the token or the frame. This matters even with only one producer:
without it, `SyslogServer` would have to guess *both* the auth shape and the
field-mapping rules from content before it's validated anything, which is
exactly the SD-vs-prefix-vs-hostname ambiguity the capture was run to avoid.
A second platform (Fly.io, Railway, …) repeats the recipe — new hostname,
cert, port, `Producer` — rather than teaching one listener to guess.
Cardinality here is hosting *platforms*, not tenants (a handful, growing
slowly), so this doesn't run into the 50-listener NLB cap that ruled out a
dedicated listener per *tenant* under the old A2 idea.

**No app-side cert management.** TLS terminates at the NLB against an **ACM
cert**, which AWS renews automatically — the api process never sees a private
key.

**No PPv2, no SNI parsing.** The capture settled auth: the token arrives
in-band, prepended in cleartext to every line, never in the SNI or a DNS
label. Producer identity is resolved by the NLB listener/port before a byte
of syslog is read, so nothing needs SNI passthrough either. NLB preserves the
real client source IP at the TCP layer on its own.

### No new NLB — **done, live in prod** (2026-09-02)

The public **`weft-nlb`** already existed (`infrastructure-catalog/stacks/asg-weft-application/main.tf`,
the `aws_lb.coordinator` resource — despite the name it is the shared public
NLB, and it **already carried a TLS listener**: gravitty's `:443`). Applied,
per producer (Render is the first):

1. `aws_lb_target_group.syslog` (`weft-syslog`) — protocol `TCP`, port 9023,
   target = coordinator ASG (which also runs gRPC :9020 and the api server
   behind its own ALB), health check on the api health port. **No new
   instance, ASG, or process** — `SyslogServer` is one more listener inside
   the same coordinator binary, colocated exactly like gRPC and the api
   server already are, and scales/fails over with it.
   `proxy_protocol_v2` was tried and removed: besides not carrying SNI (see
   *Wire capture results*), it prefixes every connection through the TG —
   including health checks, even though the health check port (9021)
   differs from the traffic port (9023) — and the coordinator's plain HTTP
   `/health` handler can't parse that prefix, so it 400s and the target
   reads `Target.FailedHealthChecks`. Confirmed live via a packet capture
   before switching it off. Target is healthy now.
2. `aws_lb_listener.syslog` — port 6514, protocol **`TLS`**,
   `certificate_arn` = ACM cert, `ssl_policy = ELBSecurityPolicy-TLS13-1-2-2021-06`,
   forward to the TG.
3. ACM cert + DNS-validation records, in the weft account (220664822695).
   Scoped `*.syslog.w3ft.net` (wildcard) — a leftover from briefly
   considering the shared-listener/SNI design mid-build (see the PPv2 result
   above); harmless since Render's own endpoint matches it fine, but could
   be narrowed to `render.syslog.w3ft.net` only as cleanup, not urgent.
4. `module.coordinator_asg.target_group_arns` — appended the new TG arn.
   This changed the ASG resource, which has an `instance_refresh` block
   (`min_healthy_percentage = 100`) — applying it triggered a real, graceful
   rolling replacement of the coordinator instance. Zero-downtime by design
   (new instance healthy before the old one drains), but worth knowing this
   ASG resource carries that side effect on any future targeted change too.
5. Route 53: `*.syslog.w3ft.net` (wildcard, same leftover-but-harmless
   reasoning as the cert) → `weft-nlb`.
6. Instance SG: allow 9023 from the NLB subnet (same rule as 9020).

A later producer repeats 1-6 on a new port (9024, …) and its own dedicated
hostname/cert (non-wildcard, learning from the point above) — this is the
reusable template, not a one-off.

> ⚠️ `weft-nlb` is **shared with gravitty** (their `:443` and `:2222` listeners
> live in a separate state). weft owns the LB and its own listeners, so adding a
> weft listener is in-bounds, but a botched apply on this LB takes gravitty
> down. Apply deliberately; see the header comment in `main.tf`.

### Auth (confirmed)

One token per namespace+env, like Heroku, scoped to the producer it was
minted for. `weft token create --for render` mints a token; the customer
pastes it into Render's Token field, which they find via the
`render.syslog.w3ft.net:6514` endpoint string the command prints. Because
`SyslogServer` already knows it's talking to Render (it's the instance bound
to port 9023), it reads the token as the literal text before the first space
on each line — no SD parsing, no per-tenant hostname, no PPv2 — and validates
it scoped to `producer="render"` via `store.ValidateAgentToken`, so a token
minted for a different producer can't be replayed here even if it leaked.
Unknown/malformed token → drop the connection before forwarding anything.
With no token configured, lines arrive with no prefix at all — decide whether
an unauthenticated stream is rejected outright or accepted into some default
quarantine namespace; leaning reject, matching the other adapters.

### Security posture

The token travels inside the TLS session on every line — never in the SNI, a
DNS label, or anywhere else visible to an on-path observer or resolver logs.
Bearer-credential model, same as the Heroku drain URL:

- Leaked-token blast radius = forged log lines into **one** namespace. No
  read access, no account access. Revoke by rotating the token.
- Endpoint is ingest-only, rate-limited per token.
- Optional source-IP allowlist against Render's egress ranges (defence-in-depth,
  not tenant auth on its own) — no published CIDR list found; the capture saw
  `74.220.48.0/24`-ish and `35.227.x.x`/`35.230.x.x` GCP addresses, not stable
  enough to allowlist without Render publishing ranges.

## Approach B — customer-run relay (fallback, zero new infra)

For a customer who will not accept the hostname-credential model. They run
[Vector](https://vector.dev) (or rsyslog) as a Render **private service**
(always-on, no public port) or on their own host:

```toml
[sources.render]
type = "syslog"
mode = "tcp"
address = "0.0.0.0:6514"

[sinks.weft]
type = "http"
inputs = ["render"]
uri = "https://ingest.w3ft.net/api/ingest"
encoding.codec = "json"
request.headers.Authorization = "Bearer wft_…"
```

This reuses the existing api ALB `:443` and the `JSONAdapter` from
`http-ingest.md` — **no new listener, no NLB change**. The token stays inside
TLS. Cost: the customer operates one more process. Axiom themselves recommend
Vector over raw syslog for exactly this reason.

Document both. Lead the prospect with A (one endpoint string, parity with
Datadog/Better Stack/Axiom); offer B when they ask about SNI/DNS exposure.

## Shared components

Both approaches — and the existing `HerokuAdapter` — need one RFC5424 parser
and one "platform proc → weft sub-app" splitter. **Shipped** as `ingest/syslog/`:

- `rfc5424.go` — `Frame` (flat weft-shaped value: `""` for an absent header, a
  best-effort `now` for an absent timestamp) + `fromRFC5424` converter. The
  RFC5424 grammar and STRUCTURED-DATA parsing (with `\" \\ \]` escape handling)
  are `github.com/leodido/go-syslog/v4`'s `rfc5424.Machine` — the parser Vector,
  Telegraf and Benthos use. The hand-rolled `parseRFC5424` in
  `api/adapter_heroku.go` (and its naive balanced-bracket SD walker) is gone.
  New direct dep: `leodido/go-syslog/v4` only (no transitive runtime deps —
  `x/text` was already present, `ragel-machinery` is only for the library's own
  streaming scanners, which we don't use).
- `framing.go` — `ParseStream(r, Framing, maxFrame, fn)`: a hand-rolled RFC 6587
  frame splitter (octet-counting + LF, `Detect` autodetects from the first
  byte) feeding each frame to the reused `rfc5424.Machine`. Hand-rolled because
  we need lenient per-frame skip semantics — leodido's own `octetcounting`
  parser aborts the whole stream on a frame whose payload isn't `<`-prefixed,
  which would drop the tail of a Heroku batch. Push API (`fn` per frame) so
  `SyslogServer` drives it incrementally over a long-lived connection.
- `proc.go` — `SplitProcID`, lifted verbatim from `api/adapter_heroku.go`.
- `producer.go` (still to build) — the seam the concern above lands on:

  ```go
  // Producer differentiates one syslog-speaking platform's frame shape from
  // another. SyslogServer is otherwise fully generic — TLS termination,
  // token auth, LF framing, RFC5424 parsing — and knows nothing
  // platform-specific. Each SyslogServer instance is constructed with
  // exactly one Producer (it owns one NLB listener/port, so it never needs
  // to guess which platform a connection is from — see Approach A); tokens
  // are validated scoped to that same producer name, so a token minted for
  // a different producer can't be replayed against this listener.
  type Producer interface {
      // MapFields turns one parsed Frame's HOSTNAME/APP-NAME/MSGID into
      // weft's Service/Kind/Instance triple. ok=false drops the frame
      // (e.g. Render's own connectivity-test ping) without forwarding it.
      MapFields(f Frame) (service, kind, instance string, ok bool)
  }
  ```

  `producer_render.go` is the first (only) implementation: `HOSTNAME` →
  `Service`; `APP-NAME`/`MSGID` split on the *last* `-` → `Kind`/`Instance`
  (`web-9nk6q` → `web`, `9nk6q`); `HOSTNAME == "render"` (the connectivity
  ping) → `ok=false`. `bld` (build-step output, undiscoverable in Render's
  docs — confirmed live in the capture) is kept as its own `Kind`, not
  folded into `web`/`cron` or dropped — customers can reasonably want to
  query build failures the same way they query anything else.

  A second producer (Fly.io, Railway, …) is a new file implementing this
  interface, a new `weft token create --for <name>` producer name, and a new
  NLB listener/cert/port per *No new NLB* above — not new `SyslogServer`
  code. **Not built now**: a second auth-extraction *shape* (this only
  implements the one confirmed token-prefix form) — if a future platform
  needs SD-embedded auth instead, that new `Producer`'s `SyslogServer`
  instance parses it differently; still no shared guessing logic, since each
  instance already knows which platform it's serving.

`HerokuAdapter` is repointed at `syslog.ParseStream` with its 11 tests green.
One deliberate behaviour change: a genuinely corrupt frame *length* (not just
bad contents) now ends parsing of that POST rather than being skipped — the
next frame's offset is unknowable — and a NILVALUE (`-`) header now flattens to
`""` instead of the literal `"-"`.

## Wire capture results

Done 2026-09-02 against a throwaway `web` service, a `cron` job, and their
build logs (harness: `scripts/capture-render-syslog.py`, throwaway host +
Let's Encrypt cert at `render-capture.w3ft.net`, torn down after). Answers:

- [x] **Auth**: neither A1 nor A2. Token is prepended as a literal cleartext
      string before every line — see *Auth (confirmed)* above.
- [x] **Framing**: always LF-delimited. Never saw octet-counting.
- [x] **`HOSTNAME`/`APP-NAME`/`PROCID`/`MSGID`**: `HOSTNAME`=service slug;
      `APP-NAME`=`MSGID`=`{kind}-{instance-id}` split on the last `-`;
      `PROCID` always `1`. Kinds seen: `web`, `cron`, `bld` (build logs —
      undocumented, real traffic). Datastore not captured (no throwaway
      Postgres stood up) — still a documented gap, not blocking.
- [x] **`<PRI>`**: real values, severity reflects `logfmt`/JSON `level` as
      documented.
- [x] **Connections / batching**: one frame per write, no batching. A
      finished cron run's output arrives as a single burst (Render buffers
      it until process exit) — all lines share one receive timestamp
      regardless of the app's own real-time spacing.
- [x] **Multi-line**: split, not joined — one full RFC5424 frame per source
      line, even mid-traceback.
- [x] **PPv2/SNI scratch test**: run for real (2026-09-02) against the
      actual deployed `weft-nlb` TLS listener, not a throwaway stand-in —
      **AWS does not forward SNI through PPv2**. The real PPv2 header
      received at the target contained only a `PP2_TYPE_CRC32C` TLV and 62
      bytes of `PP2_TYPE_NOOP` padding; no `PP2_TYPE_AUTHORITY` (0x02) TLV,
      no SNI anywhere. (Source IP preservation does work — the real client
      IP arrived correctly in the PPv2 address block.) This ruled out a
      single shared listener disambiguating producers by SNI — a design
      briefly considered mid-build — and confirmed *one listener/cert/port
      per producer* (below) as the only viable mechanism, not just the
      simplest one. Second, unrelated finding from the same investigation:
      `proxy_protocol_v2` on a target group prefixes *every* connection
      through it, including health checks, even on a different port than
      traffic — broke the coordinator's plain HTTP `/health` handler until
      removed (see *No new NLB*). Left off entirely; not needed for source
      IP either, which NLB preserves natively.

## Components to build

1. ~~**`ingest/syslog/`** — shared RFC5424 parser + framing reader, extracted
   from `HerokuAdapter`. Repoint Heroku at it.~~ **Done.** Landed as a
   `leodido/go-syslog`-backed `Frame` + `ParseStream` + `SplitProcID`; Heroku
   repointed, 11 tests green, 14 new tests in `ingest/syslog/`.
1. ~~**`ingest/syslog/producer.go` + `producer_render.go`** — the `Producer`
   interface and Render's implementation. See *Shared components*.~~ **Done.**
   `Producer` has `Name() string` (token-scoping/`--for` key) and
   `MapFields(Frame) (service, kind, instance string, ok bool)`.
   `RenderProducer.Name()` = `"render"`; `MapFields` maps HOSTNAME → Service,
   APP-NAME (falling back to MSGID) split on the last `-` → Kind/Instance,
   and drops the `HOSTNAME == "render"` connectivity ping. 7 new tests in
   `ingest/syslog/producer_render_test.go`.
1. ~~**Schema prereq**: `store.AgentToken.Producer` column~~ **Done.** Migration
   073 adds `agent_tokens.producer TEXT NOT NULL DEFAULT ''`;
   `CreateAgentToken` takes a `producer` arg (every existing caller passes
   `""`); `ValidateAgentTokenForProducer(token, producer)` is a new
   one-query check alongside the untouched producer-agnostic
   `ValidateAgentToken`. 1 new test (`store/token_producer_test.go`) pins:
   scoped token validates only for its own producer; unscoped token
   validates on `ValidateAgentToken` but never on the scoped check.
1. ~~**`api/syslog_server.go`** — `SyslogServer`: TCP accept loop on a
   configured port, strip the leading `TOKEN SP`, validate scoped to its
   configured producer, LF-framed `ingest/syslog.ParseStream`, run each
   frame through its `Producer`, forward via `coordClient.IngestStream()`,
   idle-timeout / graceful-drain handling.~~ **Done.** Constructed via
   `(*Server).NewSyslogServer(producer, port)`, reusing the `Server`'s
   store + coordinator client; `Start(ctx)` binds synchronously (fails
   fast on a port conflict) then accepts in the background; `Close()`
   (or `ctx` cancellation) closes the listener and every in-flight
   connection for graceful drain. A small `syslogConnReader` strips the
   repeating `"TOKEN SP"` prefix per line so `ingest/syslog.ParseStream`
   is reused unmodified for framing + RFC5424 parsing — no forked parser.
   Wired into `cmd/api/main.go` next to `StartUnverifiedSweep`: one
   instance, `RenderProducer`, port 9023 (`--render-syslog-port`),
   matching the NLB target group already live in prod. 4 new tests in
   `api/syslog_server_test.go` (golden path incl. the connectivity-ping
   drop, plus unscoped/wrong-producer/no-prefix rejection), race-clean.
   **Reconnect handling** is Render's responsibility, not this server's —
   it just accepts a new TCP connection the same way each time; nothing
   here needed building.
4. ~~**`weft token create --for render`** — `--for` takes any registered
   producer name, not just `render`; sets `AgentToken.Producer`. Endpoint
   string in the command output and dashboard.~~ **Done, CLI and
   dashboard.** `POST /api/tokens` takes an optional `producer` field,
   validated against a new `syslogProducers` registry (`api/handlers.go`);
   `(*Server).syslogEndpoint` derives the `<producer>.syslog.<host>:6514`
   string from `baseURL` rather than a hardcoded domain; `weft token
   create --for render` prints both. 3 new tests in
   `api/token_producer_test.go`. **Dashboard**: a new
   `GET /api/syslog-producers` endpoint exposes the same registry (name +
   endpoint) so the web token-create form (Namespaces page) can render a
   "For" selector without hardcoding platform names; `GET /api/tokens`
   now also returns `producer`/`endpoint` per token so a previously
   minted token's endpoint is still visible after the one-time reveal
   panel is dismissed. 4 new tests (`TestListSyslogProducers`,
   `TestListTokens_IncludesProducerAndEndpoint`, plus the route added to
   `TestEndpointsRequireAuth`). Verified end-to-end in a real browser
   against a locally running coordinator + Postgres: minting a
   `render`-scoped token shows the endpoint in both the reveal panel and
   the persisted token row.
5. ~~**Infra** (`asg-weft-application/main.tf`) — the *No new NLB* recipe for
   Render: `TLS` listener + TG + ACM cert on port 9023, Route 53 record, SG
   rule.~~ **Done, live in prod** (2026-09-02). See *No new NLB* above for
   what shipped and the ASG instance-refresh side effect.
6. ~~**Rate limit + bytes-in metering** — reuse the `usage_hourly` path the
   gRPC stream already updates; per-token token-bucket keyed on the
   token.~~ **Done.** Bytes-in metering needed no new code: syslog rows
   already flow through the same `IngestStream` → ring buffer → drain
   path weftd uses, so `c.Usage.Record` (and `usage_hourly`) already
   covered them. What was missing was the rate limit itself. Added
   `config.Runtime.IngestTokenBytesPerSec` /
   `IngestTokenBurstBytes` (defaults 4 MiB/s sustained, 16 MiB burst;
   either `<= 0` disables) and a new `ingestLimiter` in
   `coordinator/service/ingest_limiter.go`: an `x/time/rate`-backed
   token bucket keyed on `AgentToken.ID`, mirroring
   `api/ratelimit.go`'s per-IP limiter but persisting across
   reconnects (a customer can't dodge it by cycling connections the
   way the pre-existing `IngestStreamBudgetBytes` per-stream cap can
   be). Deliberately throttles (delays the offending row until the
   bucket refills) rather than rejecting or closing the stream —
   `IngestStreamBudgetBytes` already exists as the hard
   memory-safety valve; this is the customer-fairness layer and
   shouldn't drop data or thrash reconnects. Wired into `IngestStream`
   for both weftd and the syslog path (same RPC). 9 new tests:
   `config`'s overlay test plus `ingest_limiter_test.go` (bucket unit
   tests: disabled, within-burst, throttles, oversized row passes
   through, context-cancellation, per-token independence) and
   `ingest_ratelimit_test.go` (end-to-end `IngestStream` proving
   throttled rows are delayed but never dropped, and that disabling
   the limit is instant). Race-clean.
7. **`docs/manual/render.md`** — dashboard setup (Integrations → Observability),
   the endpoint string, the service-type → pseudofile mapping, and the Approach
   B / Vector alternative. **~0.5 day.**

Schema bootstrap / `SeenServices` onboarding is unchanged — it happens
coordinator-side on the `IngestStream` path this forwards into.

## Effort

**~5 days** for Approach A end to end. Components 1-6 (~5 days combined) are
**done**; only Component 7 (docs) remains, **~0.5 day**. Approach B is ~1 day
on top (Vector config doc + verifying the `JSONAdapter` accepts Vector's
shape) and can ship first as the unblock-the-prospect stopgap while A is
built.

## Non-goals

- **Not Render's HTTPS/JSON path.** Locked to Datadog/Loggly; not available.
- **Not a Render API integration.** We do not call Render's API to enumerate
  services or auto-configure the stream — the customer pastes one endpoint.
- **Not a second public NLB.** A listener on `weft-nlb`, or Approach B over the
  existing ALB.
- **Not per-service tokens.** One token per namespace+env, like Heroku.
- **Not multi-line reassembly here.** Confirmed moot by the capture — Render
  already splits stack traces line-by-line with a full header repeated on
  each. Nothing to reassemble.
- **Not l2met/metrics auto-routing.** Render has no l2met equivalent; its
  datastore metric lines route by service type via the normal mapping.
- **Not a multi-shape auth abstraction.** `SyslogServer` implements the one
  confirmed auth shape (leading `TOKEN SP`). A second syslog-speaking
  platform needing a different shape (SD-embedded, …) parses it in that
  producer's own `SyslogServer` instance — not built preemptively now.
- **Not one shared listener guessing the producer from content.** Producer
  identity comes from which NLB listener/port a connection lands on, decided
  by DNS before any bytes arrive — never sniffed from the token shape or the
  frame. This was a real design mistake caught during review of the first
  draft of this plan, not a hypothetical to guard against.

## Open questions

- **Datastore field mapping.** No throwaway Render Postgres/Key Value was
  captured, so `HOSTNAME`/`APP-NAME` for datastore logs is still the plan's
  original guess (`postgres`/`keyvalue` kind), not empirically confirmed.
  Low risk — same shape as `web`/`cron`, just a different kind token — but
  worth a quick capture if a real customer flags it wrong.
- **Egress allowlist.** Render's log-stream egress IPs seen in the capture
  (`74.220.48.0/24`-ish, `35.227.x.x`, `35.230.x.x` — GCP) don't look like a
  documented, stable range. No allowlist in v1 unless Render publishes one.
