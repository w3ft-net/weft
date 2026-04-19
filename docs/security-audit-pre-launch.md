# Weft pre-launch security audit

**Scope**: full architecture & codebase review of weft at `/Users/paul/dev/weft`.
**Original audit**: commit `4f4bd01`, 2026-04-18.
**Closed since**: `99f8988` (planner SQL injection, source allowlist,
IngestStream namespace-scope bypass), `989da01` (email-verification gate
for signup), `8315d9c` (coordinator gRPC JWT interceptor), `eca7eb7`
(CLI migrated off gRPC-direct — coord gRPC is now strictly internal
for api/weftd/shard paths), `30af7ae` (per-shard Heartbeat bearer
token), `eaf636e` (shared SHARD_SECRET bearer on every coord→shard
call; rogue-reach attacks on :9000 closed), and the magic-link-TOCTOU
commit landing now (atomic UPDATE ... RETURNING; sha256(token) at
rest). Items struck from the doc; everything still here is unfixed.
**Verdict**: **All four critical cross-tenant paths closed.** The
remaining findings are high/medium hardening (agent-token plaintext,
rate limits, JWT hygiene, CORS) — none are standalone data-exfil
vectors post-fix. Launching is now a product decision, not a security
one; still land the highs before scaling.

---

## TL;DR — the unignorable ones

| # | Severity | Finding | Why it matters |
|---|---|---|---|
| 1 | **Critical** | Agent tokens (`wft_…`) stored **plaintext** in Postgres | Any DB leak = fleet-wide agent impersonation |
| 2 | **High** | `/api/auth/device/poll` unauthenticated with no rate limit + 128-bit device code | DoS + narrows the window for any future code-entropy regression |
| 3 | **Medium** | JWT 30-day expiry, no revocation list, stored in `localStorage` | XSS → month-long account takeover |
| 4 | **Medium** | Production CORS silently allows `localhost:5173/5174/3000` | Subtle token-leakage path if a dev loopback is ever reachable |

The details below back each of these with file/line references and, where relevant, a working exploit payload.

---

## 1. Agent tokens stored in plaintext  **[CRITICAL]**

`store/token.go:15-45`

```go
type AgentToken struct {
    ...
    Token       string `gorm:"uniqueIndex;not null"`   // plaintext
    ...
}

func generateToken() (string, error) {
    b := make([]byte, 16)
    if _, err := rand.Read(b); err != nil { return "", err }
    return fmt.Sprintf("wft_%x", b), nil                // 128 bits entropy
}
```

Entropy is fine. Storage is not. `ValidateAgentToken` (`store/token.go:72`) looks the token up by exact match on the plaintext column.

Consequences:

- Any read-only compromise of Postgres (stolen backup, misconfigured replica, SQL injection elsewhere) yields every customer's fleet of agent tokens, each of which can write arbitrary log data to the customer's DuckDB shards via `IngestStream`.
- `SeedDevAgentToken` (`store/token.go:170`) hard-codes `wft_dev_0000000000000000` in Postgres on dev/docker startups that pass a `--customer` flag. If this is ever inadvertently run against prod Postgres, the attacker-known token becomes a live credential.
- DB comparison isn't constant-time, but given 128-bit random tokens this doesn't add real risk.

### Fix

Store `sha256(token)` as the lookup key. On validate, hash the presented token and compare. Remove the seed dev token from any code path that could touch prod (gate on `WEFT_ENV=dev` or similar).

---

## 2. Rate-limiting and abuse controls — absent everywhere  **[HIGH]**

Nothing in `api/`, `coordinator/service/`, or `shard/service/` implements rate limiting, request-size caps, or query timeouts. Concrete issues:

- `POST /api/auth/login` (`api/handlers.go:182`): one magic-link email per request, unlimited requests. Attacker spams a victim's inbox; Sendgrid quota drains; victim's mail server may reject future real mail as spam.
- `POST /api/auth/device/poll` (`api/handlers.go:1434`): unauthenticated, unrated. An attacker who learns (or guesses) a 128-bit device code gets the JWT forever. Today the code entropy is high enough — but the endpoint being unrated means any future regression (shorter codes, predictable seeding) goes straight to exploit with no detection.
- `grpc.NewServer()` with default message size (4 MiB per message) — but `IngestStream` accumulates rows into an unbounded slice in `coordinator/service/server.go:333-362` with no per-row or per-stream byte cap. A malicious agent can OOM the coordinator.
- `Shard.Query` has no context deadline (`shard/service/server.go:160`). A pathological pushdown (deep `OR`-chain, `raw LIKE '%…%'` on billions of rows) runs until the shard OOMs.
- `POST /api/shell/exec` pipelines aren't length-limited, and neither are saved shell functions (`PUT /api/functions/{name}`) or watch pipelines (`PUT /api/watches/{name}`).

### Fix

Standard per-IP / per-customer rate-limit middleware on the api (use `golang.org/x/time/rate`). gRPC `MaxRecvMsgSize(32*MiB)` + per-stream byte budget for `IngestStream`. Add a 30s default context deadline on shard `Query` via `context.WithTimeout`. Cap free-text request bodies at sensible upper bounds (1 MiB for pipelines, 10 MiB for shell functions).

---

## 3. JWT hygiene  **[MEDIUM]**

`api/auth.go:27-82`

Good news: the validator re-computes the HMAC over `parts[0] + "." + parts[1]` (line 61), so the classic `alg: none` / algorithm-confusion attacks don't apply — changing the header changes the signed material, and HS256 is the only signing path. The signature compare uses `hmac.Equal` (line 63), which is constant-time.

Not-so-good:

- 30-day expiry with no revocation. A stolen token is valid for a month; there is no denylist table or `token_version` check against the DB.
- `Tier` is in the claims but the comment at `auth.go:25-26` admits tier enforcement re-queries the DB, so stale tier isn't a risk — but `CustomerName` is also from the original JWT, and if the user is later removed from a team (`handleRemoveTeamMember` just flips `status=removed`), their JWT still contains the old `CustomerName` and nothing in `AuthMiddleware` cross-checks team membership. Removed members keep access until expiry.
- No `iss` / `aud` claims means a JWT for the api is structurally indistinguishable from a JWT for any other service that may later share the secret — a footgun for future refactors.
- Stored in `localStorage` (frontend convention per `frontend/src/lib/api.ts`). Any XSS is a full account takeover for 30 days.

### Fix

Reduce expiry to 1h; add refresh tokens stored server-side with rotation on use; check a lightweight `revoked_until` column in `AuthMiddleware` (cached). Add `iss: "weft-api"` and `aud: "weft-api"` claims and validate them. On `handleRemoveTeamMember`, additionally bump a `User.token_version` that `AuthMiddleware` cross-checks.

---

## 4. CORS and CSRF  **[MEDIUM]**

`api/middleware.go:83-108`

```go
allowed := map[string]bool{origin: true}
for _, port := range []string{"5173", "5174", "3000"} {
    allowed["http://localhost:"+port] = true
}
```

Dev origins are allowlisted **unconditionally in production**. This is usually harmless because a browser that already holds a target JWT typically wouldn't be talking to `http://localhost:5173` at all. But:

- `Access-Control-Allow-Credentials` is not set to `false`. Most browsers won't include credentials on cross-origin fetches without it, but for an SPA using `Authorization: Bearer` (as weft does), credentials mode isn't the risk — the real risk is that any local dev page on port 5173 can read JSON responses if the user has the JWT in a localStorage key it can reach via postMessage / iframe tricks. It's a small surface but indefensible.
- No CSRF tokens on any state-changing REST endpoint. Today this is mitigated by the `Authorization` header (browsers don't auto-attach it), but if you ever move JWT into a cookie, every POST/PUT/DELETE becomes CSRFable overnight.

### Fix

Gate the localhost origins on `os.Getenv("WEFT_ENV") == "dev"`. Explicitly set `Access-Control-Allow-Credentials: false`. Document that any migration to cookie-auth must land CSRF protection in the same PR.

---

## 5. Miscellaneous findings  **[LOW – MEDIUM]**

- **`InjectCustomer` substring match.** `planner/sql.go:76` uses `strings.Contains(sql, " WHERE ")` to decide where to insert the customer predicate, and the value is interpolated via `fmt.Sprintf`. Safe today — the planner never produces `" WHERE "` as a substring of any `rawExpr` or predicate, and the allowlist in `coordinator.Query` gates the app name upstream — but brittle. A future change that puts user data inside a string literal containing ` WHERE ` would silently misplace the predicate. Move to an AST-level SQL build with parameterized literals.
- **`SeedDevAgentToken`** (`store/token.go:169`) writes a known token whenever `--customer` is passed. Ensure this is never executable with a prod Postgres DSN.
- **`handleLogin` account enumeration.** Response timing differs for registered vs. unregistered emails because Sendgrid call latency dominates and only happens for registered addresses after `CreateMagicLink` returns. Minor.
- **Slack OAuth redirect URI** is derived from `s.baseURL` (`api/integrations.go`); if `baseURL` is ever affected by a Host header trust, an attacker could redirect the flow. Today `baseURL` is a fixed flag at startup, so this is theoretical.
- **`isOwner` check uses email equality**, which is case-sensitive (`store/user.go:250`). Postgres `citext` or lowercasing at write-time would remove a class of identity confusion.
- **Stripe webhook** uses `webhook.ConstructEvent` with the webhook secret (`api/handlers.go:647`) — correctly verified.
- **Cron watch pipelines and shell functions** are executed server-side through warp (`coordinator/cron.go`, `coordinator/weftcmds.go`). A malicious watch stored by a legitimate customer is a fresh attack vector every time the scheduler fires — any future SQL-construction change in those paths needs the same allowlist + escape discipline the main query path now has.
- **`grantcentral-worker.duckdb`** (a 536KB binary file) and `wef` (a 76 MiB binary) are checked into the repo root. Not a security issue per se, but whatever is in them should be reviewed and removed / moved to a storage bucket.

---

## Recommended pre-launch punch list

Blocking (must land before opening signups):

1. Hash agent tokens at rest (same sha256-lookup pattern now used for magic links).
2. gRPC `MaxRecvMsgSize` + per-stream byte budget; 30s default query deadline; rate limits on `/api/auth/login`, `/api/auth/device/poll`, and the anonymous Stripe webhook path (already length-capped, good).

Strongly recommended before first paying customer:

3. TLS (preferably mTLS) on every inter-service gRPC channel — today the shared-bearer-token flows on plaintext TCP inside the VPC, so a compromised VPC peer can sniff rows in transit. Bearer is defense-in-depth on auth, not on confidentiality.
4. JWT expiry ≤ 1h with refresh tokens; add `iss`/`aud`; add a token revocation check tied to team-member removal.
5. Production-only CORS allowlist.
6. Follow-up on migration 015's grandfather clause — once the shard fleet has rolled, add a follow-up migration 016 that NOT-NULLs `shard_records.heartbeat_token` and tightens the Heartbeat handler to reject tokenless shards. Today's code accepts them during the transition window.
7. SHARD_SECRET rotation plan. Currently a static env var; in prod it's populated by SSM/Secrets Manager. Document the rotate-and-redeploy procedure (roll coord first with dual-accept, then roll shards, then roll coord to new-only) before rotating becomes necessary.

Everything else on this list is follow-up hardening that can ship on a normal release cadence once the above is done.

— end of audit —
