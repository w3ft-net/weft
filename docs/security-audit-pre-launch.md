# Weft pre-launch security audit

**Scope**: full architecture & codebase review of weft at `/Users/paul/dev/weft`, commit `4f4bd01`.
**Date**: 2026-04-18.
**Verdict**: **Do not launch**. Four independent cross-tenant data-exfiltration paths, plus unauthenticated signup and a dozen high-impact hardening gaps. All are fixable without architectural changes.

---

## TL;DR — the unignorable ones

| # | Severity | Finding | Why it matters |
|---|---|---|---|
| 1 | **Critical** | SQL injection in planner via `grep`/`sed`/`app` name bypasses the customer WHERE clause | Any authenticated user can read every other customer's logs |
| 2 | **Critical** | Coordinator gRPC handlers trust client-supplied `customer` field; no auth | Any network-adjacent caller reads/writes any tenant, creates tokens, runs watches |
| 3 | **Critical** | Heartbeat RPC accepts any `shard_id`+`addr` with no auth | Rogue shard injection → query traffic routed to attacker-controlled node |
| 4 | **Critical** | `POST /api/auth/signup` issues a 30-day JWT for **any** email with zero verification | Account squatting / impersonation |
| 5 | **Critical** | Agent tokens (`wft_…`) stored **plaintext** in Postgres | Any DB leak = fleet-wide agent impersonation |
| 6 | **High** | Magic-link validation and consumption are not atomic | TOCTOU → single link creates multiple accounts |
| 7 | **High** | Shard gRPC unauthenticated; no TLS between any services | Lateral movement inside VPC = full DuckDB read/write |
| 8 | **High** | `/api/auth/device/poll` unauthenticated with no rate limit + 128-bit device code | DoS + narrows the window for any future code-entropy regression |
| 9 | **Medium** | JWT 30-day expiry, no revocation list, stored in `localStorage` | XSS → month-long account takeover |
| 10 | **Medium** | Production CORS silently allows `localhost:5173/5174/3000` | Subtle token-leakage path if a dev loopback is ever reachable |

The details below back each of these with file/line references and, where relevant, a working exploit payload.

---

## 1. Cross-tenant SQL injection in the query planner  **[CRITICAL — data exfil]**

### 1a. `grep` / `sed` patterns are not quote-escaped

`planner/grep.go:74`

```go
return fmt.Sprintf("raw %s '%%%s%%'", likeOp, escapeSQLLike(pattern))
```

`planner/awk.go:393`

```go
func escapeSQLLike(s string) string {
    s = strings.ReplaceAll(s, "%", "\\%")
    s = strings.ReplaceAll(s, "_", "\\_")
    return s          // ← single quotes NOT escaped
}
```

`escapeSQLLike` escapes `%` and `_` but **not single quotes**. The resulting predicate is emitted without parentheses (`planner/planner.go:196-200` returns the predicate bare), so the attacker can terminate the string literal and append arbitrary SQL.

**Proof of concept** (as an authenticated user from any tenant):

```
cat myapp.log | grep "x' OR 1=1 --"
```

becomes

```sql
SELECT line_num, raw FROM logs
 WHERE customer = 'victim-co'              -- InjectCustomer prepends this
   AND (app = 'myapp' OR app LIKE 'myapp/%')
   AND raw LIKE '%x' OR 1=1 --%'
```

By SQL precedence (`AND` binds tighter than `OR`), this is `(customer=… AND app=… AND raw LIKE '%x') OR TRUE`. Every row in every shard of every customer is returned.

The same injection works via `sed -n '/x' OR 1=1 --/p'` (`planner/sed.go:52` uses the same unescaped helper).

### 1b. The pseudofile app name is interpolated raw

`planner/planner.go:57-62`

```go
if info.source.app != "" {
    if strings.Contains(info.source.app, "/") {
        sql.addPredicate(fmt.Sprintf("app = '%s'", info.source.app))
    } else {
        sql.addPredicate(fmt.Sprintf("(app = '%s' OR app LIKE '%s/%%')", info.source.app, info.source.app))
    }
}
```

`info.source.app` is derived from the user-typed pseudofile name (`planner/parse.go:161` `extractApp`) with zero escaping.

**Proof of concept**:

```
cat "a') OR 1=1 --.log"
```

generates

```sql
SELECT line_num, raw FROM logs
 WHERE customer = 'victim-co'
   AND (app = 'a') OR 1=1 --' OR app LIKE 'a') OR 1=1 --/%')
```

The first `)` closes the group early, `OR 1=1` wins, `--` eats the rest. Full cross-tenant read.

### Fix

One line per site: wrap every user-derived value in `escapeSQLString` before interpolation, and parenthesize every pushed predicate. Better still, move the pushed predicates into an `AND` chain of `?`-parameterized clauses using DuckDB's prepared statements — `s.db.Query(q, args...)` is already `database/sql`, so it's free. The `InjectCustomer` string dance (`planner/sql.go:74-86`) should also be replaced with a parameter; today it happens to be safe against malicious *customer* strings but it is stringly-typed and fragile.

### 1c. Honourable mention — `InjectCustomer` substring match

`planner/sql.go:76` uses `strings.Contains(sql, " WHERE ")` to decide where to slot the customer predicate. The planner never produces a stray `" WHERE "` substring today, but a future change to `rawExpr` that quotes user data containing the literal string ` WHERE ` would silently insert the customer predicate inside a string literal. Replace with a proper AST-level build.

---

## 2. Coordinator gRPC — unauthenticated admin surface  **[CRITICAL]**

`cmd/coordinator/main.go:281` instantiates `grpc.NewServer()` with no interceptors, no credentials, no TLS. The `coordinator/service/server.go` handlers then trust a client-supplied `Customer` field on every request:

| RPC | File:line | Handler behaviour |
|---|---|---|
| `Query` | `server.go:32` | Reads logs for `req.Customer` — no bearer check |
| `ListApps` | `server.go:108` | Lists apps for `req.Customer` |
| `AddSource` / `RemoveSource` / `ListSources` | `server.go:144-203` | Mutates log-group config for `req.Customer` |
| `CreateToken` | `server.go:243` | **Calls `FindOrCreateCustomer(req.Customer)`** — creates the customer if missing, mints a plaintext `wft_…` token |
| `ListTokens` / `RevokeToken` | `server.go:263-296` | Lists / revokes tokens by customer |
| `SaveFunction` / `GetFunction` / `DeleteFunction` | `server.go:491-539` | CRUD on `/bin/` shell functions |
| `SaveWatch` / `GetWatch` / `DeleteWatch` / `GetWatchResults` | `server.go:543-626` | CRUD on cron watches — these execute pipelines server-side |
| `GetUsage` | `server.go:443` | Metered billing data |
| `Heartbeat` | `server.go:632` | See §3 below |

The `weft` CLI's gRPC backend makes this concrete (`shell/backend_grpc.go:23-95`):

```go
resp, err := b.client.SaveFunction(context.Background(), &pb.SaveFunctionRequest{
    Customer: b.customer, Name: name, Source: source,
})
```

`b.customer` is a client-side string. There is no token verification on the CLI side of this channel. Multi-tenancy on the CLI path is honour-system.

The api service *does* enforce JWT on its REST endpoints (`api/handlers.go`) and uses `claims.CustomerName` when it calls the coordinator. But the coordinator itself doesn't know that — any process that can reach port 9020 bypasses the API layer entirely.

**Exposure**: in prod the coordinator listens on `:9020` (no interface binding). Whether this is reachable from the internet depends on the ALB + security group configuration in `infrastructure-{live,catalog}` (not in this repo). Even if the public boundary is airtight today, a single misconfigured SG rule, a hijacked shard EC2 instance, or a compromised agent host in the VPC turns this into full cross-tenant access.

Only `IngestStream`, `Authenticate`, `SubmitSample`, and `GetSourceStatus` validate a bearer token (see `authenticateStream` at `server.go:379`). Everything else is wide open.

### Fix

Add a `grpc.UnaryInterceptor` / `StreamInterceptor` pair on both coordinator and shard servers. Require a bearer token on every RPC (JWT for user-path, agent token for ingest-path, per-shard token for lifecycle/Heartbeat). Resolve the customer from the verified principal — **never** trust `req.Customer`. Enable TLS between all services; mTLS is the correct endpoint but even plain TLS + token closes most of this.

---

## 3. Heartbeat RPC can register rogue shards  **[CRITICAL]**

`coordinator/service/server.go:632-653`

```go
func (s *Server) Heartbeat(_ context.Context, req *pb.ShardHeartbeatRequest) (*pb.HeartbeatResponse, error) {
    hb := store.ShardHeartbeat{
        ShardID:    req.ShardId,   // caller-supplied
        Addr:       req.Addr,      // caller-supplied
        InstanceID: req.InstanceId,
        State:      req.State,
        ...
    }
    if s.Coord.Provisioner != nil {
        go s.Coord.Provisioner.ProcessHeartbeat(hb)   // christen / rebind / adopt
    }
    return &pb.HeartbeatResponse{}, nil
}
```

No auth, no signature, no rate limiting, no binding check that the caller is at the address it claims to be at. `ProcessHeartbeat` will:

- for an empty `ShardId`, christen a new shard (mint id, write the Docker launcher / ASG tag, `Configure` RPC back to the address),
- for a known id whose address changed, **rebind** the registry entry to the new address.

An attacker with network reach to `:9020` can send a single heartbeat with `ShardId` matching a real shard and `Addr` pointing to their own machine. The next `MatchingShards` lookup routes Query RPCs to the attacker. Since every tenant's rows flow through `shard.Query(sql)` with the sql already containing the customer filter — but the attacker implements a rogue shard that returns everything it's asked *plus* records it — this is silent, persistent cross-tenant data exfiltration.

### Fix

Either (a) mTLS with per-shard client certs provisioned at launch, or (b) a per-shard bearer token minted when the provisioner christens and stored in the shard's `shard.json`, validated on every heartbeat. Until then, do not enable the provisioner in prod without a VPC whose `:9020` security group is provably locked down to the coordinator's own subnet.

---

## 4. `/api/auth/signup` — no email verification  **[CRITICAL]**

`api/handlers.go:207-290`, registered as a public route at `api/handlers.go:93`:

```go
mux.HandleFunc("POST /api/auth/signup", s.handleSignup)
```

The handler accepts `{email, org_name, tier}`, checks that `tier` is a known Stripe price, checks the email doesn't already exist, and **issues a 30-day JWT** bound to the freshly created (email, customer, owner) triple — without ever sending an email, generating a magic link, or otherwise proving ownership of the email.

```go
// handleSignup, line 272-289
user.CustomerName = customerName
jwt, err := GenerateJWT(user, "trial", s.jwtSecret)
...
json.NewEncoder(w).Encode(map[string]any{ "token": jwt, ... })
```

**Impact**:

- Pre-registering a victim's email under an attacker-chosen org, then using the token to create agent tokens, watches, Slack integrations, and saved queries. When the real user tries to sign up, they get `account already exists`.
- The victim's genuine magic-link flow (`handleVerify`) can still log them in as the existing user, but the attacker is the `owner` team member (see `handleSignup:260-270`) and controls all agent tokens they minted in the interim.
- The Stripe trial subscription is created for the attacker's org; if the attacker then uses `/api/billing/setup-intent` with the stolen JWT and a prepaid / stolen card, they can lock the victim out of ever creating that org name.

The same handler is the *only* flow currently wired to the signup form in the frontend (`frontend/src/lib/api.ts:32`), so this is live code.

### Fix

Delete the JWT issuance from `handleSignup` entirely. Either make signup identical to `handleLogin` (send a magic link, verify, then create the customer on `handleVerify`) or require Stripe checkout completion as proof. Issuing a JWT before the email round-trip cannot be made safe.

---

## 5. Agent tokens stored in plaintext  **[CRITICAL]**

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

## 6. Magic-link TOCTOU  **[HIGH]**

`store/user.go:184-195`

```go
func (s *Store) ValidateMagicLink(token string) (string, error) {
    var link MagicLink
    if err := s.db.Where("token = ? AND used = ? AND expires_at > ?", token, false, time.Now()).First(&link).Error; err != nil {
        return "", fmt.Errorf("invalid or expired magic link")
    }
    return link.Email, nil
}

func (s *Store) ConsumeMagicLink(token string) {
    s.db.Model(&MagicLink{}).Where("token = ?", token).Update("used", true)
}
```

`api/handlers.go:302-385` is the only caller; it does `Validate → … long path through user creation, subscription, team member … → Consume`. Two concurrent `POST /api/auth/verify` requests with the same token both see `used=false`, both succeed, and both produce JWTs. Worse, if the same email has never registered, the two requests race to `CreateUser` + `CreateSubscription`, producing duplicate rows (the `User` unique index is `(email, customer_id)`, so a second customer with the same org name *would* be rejected, but the `verify` path only creates the customer on brand-new users — in the race both may pass the exists check).

Magic links are also stored plaintext (`store/user.go:58`). Same fix as agent tokens.

### Fix

Merge validate+consume into a single `UPDATE ... WHERE used=false AND expires_at > now() RETURNING email` — one SQL statement, atomic. Store `sha256(token)` rather than plaintext.

---

## 7. Shard gRPC + inter-service trust  **[HIGH]**

`cmd/shard/main.go:57` — `grpc.NewServer()`, no auth, no TLS. The handler file `shard/service/server.go` accepts:

- `Query(QueryRequest)` — takes a raw SQL string and executes it against DuckDB. Anyone with network reach to `:9000` gets `SELECT * FROM logs` unfiltered by customer, because only the coordinator is expected to construct a customer-safe SQL string.
- `Configure(ConfigureRequest)` — assigns the shard's identity. Protected by a `shard != nil` check, so a live shard can't be repurposed — but a fresh EBS volume's shard is `nil` until christen, so there's a race window between instance launch and the coordinator's `Configure` RPC where an attacker can claim the shard under an identity of their choosing.
- `Ingest` / `IngestStream` — insert arbitrary rows, bypassing the coordinator's `row.Customer = authCustomer` overwrite in `coordinator/service/server.go:343`. An attacker who can reach a shard directly can plant rows attributed to any customer, polluting query results and (if they control the ingested JSON) driving false positives through the customer's watches / Slack integrations.

The coordinator's outbound shard client also uses `insecure.NewCredentials()` (see `cmd/shard/main.go:117` for the analogous heartbeat path). A network-in-the-middle attacker on the VPC sees every row of every query in cleartext.

### Fix

TLS on all gRPC listeners. At minimum, shared-secret bearer interceptors between coordinator ↔ shard and coordinator ↔ compute, so the trust boundary isn't "whatever the VPC security group does today."

---

## 8. Rate-limiting and abuse controls — absent everywhere  **[HIGH]**

Nothing in `api/`, `coordinator/service/`, or `shard/service/` implements rate limiting, request-size caps, or query timeouts. Concrete issues:

- `POST /api/auth/login` (`api/handlers.go:182`): one magic-link email per request, unlimited requests. Attacker spams a victim's inbox; Sendgrid quota drains; victim's mail server may reject future real mail as spam.
- `POST /api/auth/device/poll` (`api/handlers.go:1434`): unauthenticated, unrated. An attacker who learns (or guesses) a 128-bit device code gets the JWT forever. Today the code entropy is high enough — but the endpoint being unrated means any future regression (shorter codes, predictable seeding) goes straight to exploit with no detection.
- `grpc.NewServer()` with default message size (4 MiB per message) — but `IngestStream` accumulates rows into an unbounded slice in `coordinator/service/server.go:333-362` with no per-row or per-stream byte cap. A malicious agent can OOM the coordinator.
- `Shard.Query` has no context deadline (`shard/service/server.go:160`). A pathological pushdown (deep `OR`-chain, `raw LIKE '%…%'` on billions of rows) runs until the shard OOMs.
- `POST /api/shell/exec` pipelines aren't length-limited, and neither are saved shell functions (`PUT /api/functions/{name}`) or watch pipelines (`PUT /api/watches/{name}`).

### Fix

Standard per-IP / per-customer rate-limit middleware on the api (use `golang.org/x/time/rate`). gRPC `MaxRecvMsgSize(32*MiB)` + per-stream byte budget for `IngestStream`. Add a 30s default context deadline on shard `Query` via `context.WithTimeout`. Cap free-text request bodies at sensible upper bounds (1 MiB for pipelines, 10 MiB for shell functions).

---

## 9. JWT hygiene  **[MEDIUM]**

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

## 10. CORS and CSRF  **[MEDIUM]**

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

## 11. Miscellaneous findings  **[LOW – MEDIUM]**

- **`InjectCustomer` uses `fmt.Sprintf` + string substitution.** Safe today (single quotes are doubled), but the decision tree in `planner/sql.go:74-86` is brittle. Move to parameterized queries.
- **`SeedDevAgentToken`** (`store/token.go:169`) writes a known token whenever `--customer` is passed. Ensure this is never executable with a prod Postgres DSN.
- **`handleLogin` account enumeration.** Response timing differs for registered vs. unregistered emails because Sendgrid call latency dominates and only happens for registered addresses after `CreateMagicLink` returns. Minor.
- **Slack OAuth redirect URI** is derived from `s.baseURL` (`api/integrations.go`); if `baseURL` is ever affected by a Host header trust, an attacker could redirect the flow. Today `baseURL` is a fixed flag at startup, so this is theoretical.
- **`isOwner` check uses email equality**, which is case-sensitive (`store/user.go:250`). Postgres `citext` or lowercasing at write-time would remove a class of identity confusion.
- **Stripe webhook** uses `webhook.ConstructEvent` with the webhook secret (`api/handlers.go:647`) — correctly verified.
- **Ingest rows force `row.Customer = authCustomer`** in `coordinator/service/server.go:343` — this part is correctly defended. `row.App` is not overwritten, so an agent with a token for namespace `prod` can label rows under namespace `staging` of the same customer; this is a medium-severity namespace-scope bypass but stays within the customer boundary.
- **Cron watch pipelines and shell functions** are executed server-side through warp (`coordinator/cron.go`, `coordinator/weftcmds.go`). Any SQL-injection fix must also apply here, because a malicious watch stored by a legitimate customer is still a fresh attack vector every time the scheduler fires.
- **`grantcentral-worker.duckdb`** (a 536KB binary file) and `wef` (a 76 MiB binary) are checked into the repo root. Not a security issue per se, but whatever is in them should be reviewed and removed / moved to a storage bucket.

---

## Recommended pre-launch punch list

Blocking (must land before opening signups):

1. Parameterize (or properly escape with `escapeSQLString` + parenthesize) every predicate in `planner/grep.go`, `planner/sed.go`, `planner/planner.go`. Add regression tests with `'`, `--`, `)`, `)--`, and mixed grep+app pushdown inputs. Verify by asserting `customer = 'victim'` is always the outermost-left AND-operand of the final SQL.
2. Add gRPC auth interceptors on the coordinator. Validate a bearer token on every RPC and derive `customer` from the verified principal, never from the request. Keep `IngestStream` using agent tokens; add user JWTs (or a separate api-issued service token) to every other RPC.
3. Delete the JWT issuance in `handleSignup`. Route signup through the magic-link flow.
4. Hash agent tokens and magic-link tokens at rest; make magic-link validate+consume a single atomic UPDATE.
5. Heartbeat auth (mTLS or per-shard bearer token). No registry mutation on unauthenticated heartbeats.
6. gRPC `MaxRecvMsgSize` + per-stream byte budget; 30s default query deadline; rate limits on `/api/auth/login`, `/api/auth/device/poll`, and the anonymous Stripe webhook path (already length-capped, good).

Strongly recommended before first paying customer:

7. TLS (preferably mTLS) on every inter-service gRPC channel.
8. JWT expiry ≤ 1h with refresh tokens; add `iss`/`aud`; add a token revocation check tied to team-member removal.
9. Production-only CORS allowlist.
10. Audit and document the network exposure of `:9000` (shard), `:9010` (compute), `:9020` (coordinator). These should be reachable only from the coordinator's own security group and, for `:9020`, from the api host.

Everything else on this list is follow-up hardening that can ship on a normal release cadence once the above is done.

— end of audit —
