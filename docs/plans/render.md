# Plan: Render.com log ingestion

A narrow adapter for customers hosted on [Render](https://render.com). Render's
only off-platform log path is **log streams** — RFC5424 syslog over TLS/TCP.
It is not HTTP, so it does not fit the `Adapter` interface in
[`http-ingest.md`](http-ingest.md); it needs a long-lived TLS syslog listener.
This plan adds that listener **without a new load balancer** (a listener on the
existing `weft-nlb`), plus a zero-new-infra fallback for security-sensitive
customers.

## Status

🟡 In progress. Triggered by a live prospect on Render. **Component 1
(`ingest/syslog/`) has shipped** — see *Shared components* below. The rest is
blocked on a one-time **wire capture** (see *Pre-implementation* below) to
decide A1 vs A2 and confirm framing / field layout against a real Render
service.

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

Render annotates each line with the **service slug**, the **service type**
(`web`, `cron`, `worker`, `pserv`, …), and a **stable instance id**. Exact
mapping onto `HOSTNAME` / `APP-NAME` / `PROCID` / `MSGID` is not documented and
varies — the `<0>` priority above is almost certainly a doc placeholder (facility
0 / severity 0 is nonsensical for an app log). The capture resolves this.
Render maps a `level` field to syslog severity only when the app emits `logfmt`
or `json`. Datastore logs (Render Postgres, Key Value) are included.

### Auth — the hard part

Render carries the "Token" field into the stream **per named provider**, in that
provider's own shape: Better Stack gets `[logtail@11993 source_token="…"]` in
STRUCTURED-DATA, Coralogix `[coralogix@… private_key="…"]`, New Relic prepends
the raw key *before* `<PRI>`. There is no universal format and **no provider
selector** — just endpoint-hostname matching. For an endpoint Render does not
recognize, the evidence is that it sends **plain RFC5424 with `-` in the SD
slot and the token nowhere**.

This is a known dead end. Axiom — a serious observability vendor with the
identical problem — resolves it by making the **endpoint hostname itself the
credential**: `https://opbizplsf8klnw.ingress.axiom.co`, wildcard cert, and
*"Consider this URL as secret information because syslog doesn't support
additional authentication such as API tokens."* That is the model Approach A
adopts. It is not security-by-obscurity: a high-entropy random label is a bearer
credential, the same trust model as an S3 presigned URL or the Heroku drain URL
weft already ships (`drain.w3ft.net/api/ingest/heroku/wft_<token>`).

## Approach A — direct TLS syslog listener (recommended)

```
Render service  ──TLS 6514──►  weft-nlb :6514  (TLS listener, ACM cert — ACM renews)
                                      │  terminates TLS, PPv2 header → target
                                      ▼
                        api instance :9023  (RenderSyslogServer)
                          - reads tenant identity (see "Auth" — capture decides)
                          - label/token → customer/namespace/env
                          - octet-counted RFC5424 frames → []*pb.LogRow
                                      │
                                      ▼
                        coordClient.IngestStream()   ← same path weftd + Heroku take
```

**No app-side cert management.** TLS terminates at the NLB against an **ACM
cert**, which AWS renews automatically — the api process never sees a private
key. This is the deciding constraint: the app-side-TLS variant (app reads
`ClientHelloInfo.ServerName` directly, no PPv2) is cleaner in code but puts a
`*.render.w3ft.net` cert-renewal chore on the fleet, which we are not doing.

### No new NLB

The public **`weft-nlb`** already exists (`infrastructure-catalog/stacks/asg-weft-application/main.tf`,
the `aws_lb.coordinator` resource — despite the name it is the shared public
NLB, and it **already carries a TLS listener**: gravitty's `:443`). Add,
mirroring that:

1. `aws_lb_target_group.render_syslog` — protocol `TCP`, port 9023, target =
   coordinator ASG (which also runs the api server), health check on the api
   health port. `proxy_protocol_v2 = true`.
2. `aws_lb_listener.render_syslog` — port 6514, protocol **`TLS`**,
   `certificate_arn` = a new ACM cert (see Auth for whether it is a wildcard),
   `ssl_policy = ELBSecurityPolicy-TLS13-1-2-2021-06`, forward to the TG.
3. ACM cert + DNS-validation records, in the weft account (220664822695).
4. `module.coordinator_asg.target_group_arns` — append the new TG arn.
5. Route 53: the endpoint hostname → `weft-nlb`.
6. Instance SG: allow 9023 from the NLB subnet (same rule as 9020).

> ⚠️ `weft-nlb` is **shared with gravitty** (their `:443` and `:2222` listeners
> live in a separate state). weft owns the LB and its own listeners, so adding a
> weft listener is in-bounds, but a botched apply on this LB takes gravitty
> down. Apply deliberately; see the header comment in `main.tf`.

### Auth — one of two shapes, decided by the capture

Render sends the "Token" field only in a recognized provider's format; for an
unrecognized endpoint the working assumption is it sends nothing tenant-
identifying. The capture confirms which case we're in:

**A1 — Render *does* put something tenant-identifying in STRUCTURED-DATA.**
Then it's clean: one shared hostname `render.w3ft.net:6514`, one non-wildcard
ACM cert, no PPv2 needed. `weft token create --for render` mints a token; the
customer pastes it into Render's Token field; `RenderSyslogServer` reads it out
of the SD element on each frame → `store.ValidateAgentToken`. This is the
Better-Stack/Sumo model and the one to hope for.

**A2 — Render sends `-` for SD (likely).** Per-tenant hostname carries the
identity, exactly as [Axiom does for Render](https://axiom.co/docs/send-data/render):
- `weft token create --for render` mints a token and a stable opaque label
  (`k3n8x2p9` — base32 of 64 random bits, never the customer name). Endpoint:
  `k3n8x2p9.render.w3ft.net:6514`. Needs a `*.render.w3ft.net` **wildcard** ACM
  cert on the listener.
- The NLB terminates TLS and, with `proxy_protocol_v2` on the TG, prepends a
  PPv2 header. `RenderSyslogServer` parses it, reads the SNI from the
  `PP2_TYPE_AUTHORITY` (0x02) TLV, strips `.render.w3ft.net`, looks the label
  up. Unknown/malformed → drop the connection before a byte of syslog.
- **Verify first (15-min scratch test, no Render needed):** stand up a throwaway
  NLB TLS listener + PPv2 TG in front of a netcat target and confirm the SNI
  actually arrives in the AUTHORITY TLV. If AWS does not forward it, fall back
  to a per-tenant **listener** (`render.w3ft.net:6514` + SNI cert selection is
  out; a dedicated port per tenant hits the 50-listener NLB cap fast) — at
  which point Approach B becomes the real answer.
- One token = one namespace+env; many Render services behind one endpoint is
  the customer's grouping choice, like Heroku.

### Security posture (A2 only)

The label rides in the TLS ClientHello SNI (cleartext) and as a DNS label, so it
is visible to on-path observers and lands in resolver / passive-DNS logs.
Bounded:

- Label is high-entropy and opaque — not tied to tenant identity.
- Leaked-endpoint blast radius = forged log lines into **one** namespace. No
  read access, no account access. Revoke by rotating the label.
- Endpoint is ingest-only, single namespace/env, rate-limited per token.
- Optional source-IP allowlist against Render's published egress ranges
  (defence-in-depth; not tenant auth on its own).
- Alert on a label's source IP changing.

A1 has none of this exposure — another reason to check the capture before
building.

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
  which would drop the tail of a Heroku batch. Push API (`fn` per frame) so the
  Render listener drives it incrementally over a long-lived connection.
- `proc.go` — `SplitProcID`, lifted verbatim from `api/adapter_heroku.go`.
- Render field mapping (still to build): `HOSTNAME`(slug) → `Service`,
  service-type token → `Kind` (`web`/`cron`/`worker`/`pserv`; datastore →
  `postgres`/`keyvalue`), instance id → `Instance`. Mirrors `SplitProcID`.

`HerokuAdapter` is repointed at `syslog.ParseStream` with its 11 tests green.
One deliberate behaviour change: a genuinely corrupt frame *length* (not just
bad contents) now ends parsing of that POST rather than being skipped — the
next frame's offset is unknowable — and a NILVALUE (`-`) header now flattens to
`""` instead of the literal `"-"`.

## Pre-implementation: wire capture

Run **before** writing the parser. Harness already built:
`scripts/capture-render-syslog.py` (TLS sink, records every recv chunk,
prints a framing + RFC5424 + SD-ID analysis).

1. Stand up the harness on a throwaway public host with a **real** cert
   (`render-capture.w3ft.net`, Let's Encrypt DNS-01 via Route 53 — self-signed
   will be rejected).
2. Deploy a throwaway Render service (one `web`, one `cron`, one `worker`, plus
   a Render Postgres) and point a log stream at it — once **with** a token,
   once **without**.
3. `python3 scripts/capture-render-syslog.py --analyze capture-*.jsonl`.

Answers we need locked before coding:

- [ ] **Does an unrecognized endpoint get any tenant-identifying
      STRUCTURED-DATA?** This picks **A1** (SD token — clean, no wildcard, no
      PPv2, no SNI exposure) vs **A2** (per-tenant hostname). Run both the
      with-token and without-token stream configs.
- [ ] Framing: octet-counted (expected) vs LF.
- [ ] Exact `HOSTNAME`/`APP-NAME`/`PROCID`/`MSGID` per service type + datastore.
- [ ] Real `<PRI>` values, or does Render send `<0>`?
- [ ] One connection or many; one frame per write or batched.
- [ ] Multi-line (stack trace) — split or joined.
- [ ] (A2 only, no Render needed) scratch NLB TLS listener + PPv2 TG → confirm
      the SNI arrives in the `PP2_TYPE_AUTHORITY` TLV.

## Components to build

1. ~~**`ingest/syslog/`** — shared RFC5424 parser + framing reader, extracted
   from `HerokuAdapter`. Repoint Heroku at it.~~ **Done.** Landed as a
   `leodido/go-syslog`-backed `Frame` + `ParseStream` + `SplitProcID`; Heroku
   repointed, 11 tests green, 14 new tests in `ingest/syslog/`.
2. **`api/render_syslog.go`** — `RenderSyslogServer`: TCP accept loop on :9023,
   PPv2 header parse (A2) or SD-token read (A1), token auth, octet-counted frame
   reader, row mapping, forward via `coordClient.IngestStream()`, idle-timeout /
   graceful-drain / reconnect handling. Started from `cmd/api/main.go` next to
   `StartUnverifiedSweep`. **~2 days.**
3. **`weft token create --for render`** — token (+ label for A2) derivation and
   storage, endpoint string in the command output and dashboard. **~0.5 day.**
4. **Infra** (`asg-weft-application/main.tf`) — `TLS` listener + PPv2 TG on
   `weft-nlb`, ACM cert (`render.w3ft.net` for A1, `*.render.w3ft.net` for A2),
   Route 53 record, SG rule. **~0.5–1 day** + a careful apply on the
   gravitty-shared LB.
5. **Rate limit + bytes-in metering** — reuse the `usage_hourly` path the gRPC
   stream already updates; per-token token-bucket keyed on the label. **~0.5
   day.**
6. **`docs/manual/render.md`** — dashboard setup (Integrations → Observability),
   the endpoint string, the service-type → pseudofile mapping, and the Approach
   B / Vector alternative. **~0.5 day.**

Schema bootstrap / `SeenServices` onboarding is unchanged — it happens
coordinator-side on the `IngestStream` path this forwards into.

## Effort

**~5 days** for Approach A end to end, + the capture (a few hours once a Render
trial and cert are in place). Component 1 (~1 day) is **done**, leaving ~4.
Approach B is ~1 day on top (Vector config doc + verifying the `JSONAdapter`
accepts Vector's shape) and can ship first as the unblock-the-prospect stopgap
while A is built.

## Non-goals

- **Not Render's HTTPS/JSON path.** Locked to Datadog/Loggly; not available.
- **Not a Render API integration.** We do not call Render's API to enumerate
  services or auto-configure the stream — the customer pastes one endpoint.
- **Not a second public NLB.** A listener on `weft-nlb`, or Approach B over the
  existing ALB.
- **Not per-service tokens.** One token per namespace+env, like Heroku.
- **Not multi-line reassembly here.** If Render splits stack traces, that is the
  same problem as [`weftd-multiline`](weftd-multiline.md) and shares its
  solution — do not special-case it in the syslog server.
- **Not l2met/metrics auto-routing.** Render has no l2met equivalent; its
  datastore metric lines route by service type via the normal mapping.

## Open questions

- **A1 vs A2** — the capture decides. A1 (Render puts a token in SD) removes the
  wildcard cert, the PPv2 parser, and the SNI exposure. Build nothing until we
  know.
- **PPv2 SNI TLV** (A2 only). Not documented cleanly by AWS for NLB TLS
  listeners; verify with the scratch test before committing. If the NLB does not
  forward the SNI, A2 is not viable and Approach B is the answer.
- **PPv2 parser** (A2 only). Hand-roll the ~40-line AUTHORITY-TLV parse, or take
  `github.com/pires/go-proxyproto`? Not a hot path (once per connection) — lean
  to the library for correctness.
- **Cert scope.** `*.render.w3ft.net` is one wildcard. If we later add Fly.io /
  Railway on the same mechanism, is it `*.render.w3ft.net` +
  `*.fly.w3ft.net`, or a shared `*.logs.w3ft.net`? A shared parent means one
  cert but a label collision surface across platforms. Lean per-platform.
- **Egress allowlist.** Does Render publish stable egress IP ranges for log
  streams? If so, wire the optional source-IP check in v1; if not, note it as
  unavailable.
