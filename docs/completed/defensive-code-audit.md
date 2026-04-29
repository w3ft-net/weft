# Defensive Code Audit

## Summary

The codebase is heavily contaminated with "is the dependency wired up?" guards
on fields the constructor / startup sequence does (or should) guarantee. The
worst offender is `Coordinator.Store`, which has 5 user-visible nil checks plus
a cascade of dependent guards (`AlertContext.Store`, `Server.Store`,
`s.coordClient`, `Server.cfgPtr`, `Server.limiter`) that all model a "what if
the DB never came up?" failure mode that doesn't exist in production — the
intended behaviour, per CLAUDE.md and Paul's feedback memory, is fail-fast on
RDS unreachability. A second, smaller cluster of dead code is the
`if claims == nil { 401 }` pattern in 13 HTTP handlers, all of which sit
behind `AuthMiddleware` that has already 401'd on a missing/invalid token; the
middleware is the only thing that can write claims into the request context,
so the post-middleware nil branch is unreachable. Stripping these would remove
~40 lines of misleading guards and force one real fix in
`cmd/coordinator/main.go` (currently logs a warning and proceeds with
`db = nil` when `store.Open` fails — that is the actual source of the
"defensive" anxiety throughout the rest of the package).

---

## 1. Nil checks on always-initialized dependencies

### 1a. `Coordinator.Store` (the triggering example)

`cmd/coordinator/main.go:112-117` opens the store, but on failure logs
`"warning: config store unavailable"` and continues with `db = nil`. That
single boot-time decision is the only reason any of the following guards
"protect" anything. Once that becomes a fatal, every check below is dead.

- **`coordinator/coordinator.go:343-345`** — `if c.Notifier != nil { ctx.Notifier = c.Notifier }` inside `QueryForWatch`.
  ```go
  if c.Notifier != nil {
      ctx.Notifier = c.Notifier
  }
  ```
  `Notifier` is set in `cmd/coordinator/main.go:291` only when `db != nil`; when `db == nil` it stays nil. With a fatal-on-store-open, `Notifier` is always set. Drop the guard, assign unconditionally.

- **`coordinator/coordinator.go:350-354`** — name-resolution helper inside `QueryForWatch`:
  ```go
  if c.Store != nil {
      if cust, err := c.Store.GetCustomerByID(watch.CustomerID); err == nil {
          custName = cust.Name
      }
  }
  ```
  `QueryForWatch` is only ever called from `CronExecutor.executeWatch` (`coordinator/cron.go:158`), and `CronExecutor` itself is only constructed in `cmd/coordinator/main.go:294-298` inside the `if db != nil` block. The Store cannot be nil here. Drop the outer guard. The inner `err == nil` swallow is also category 3 below.

- **`coordinator/coordinator.go:369-373`** — customer name → ID resolution at the top of `query()`:
  ```go
  if c.Store != nil && customer != "" {
      if cust, err := c.Store.GetCustomerByName(customer); err == nil {
          custID = cust.ID
      }
  }
  ```
  Same root cause — `c.Store` can only be nil when the boot warned. Once boot is fatal, the `c.Store != nil` half is dead. The `err == nil` silent fallback is category 3.

- **`coordinator/coordinator.go:434-436`** — duplicate of 343 inside `query()` when building an interactive `AlertContext`:
  ```go
  if c.Notifier != nil {
      alertCtx.Notifier = c.Notifier
  }
  ```
  Comment above it explains why typed-nil would be a bug — that's correct, but the right answer is "Notifier is always non-nil," not "guard the assignment."

- **`coordinator/coordinator.go:462-474`** — `executeQuery` source-app allowlist:
  ```go
  if plan.SourceApp != "" && c.Store != nil {
      cust, err := c.Store.GetCustomerByName(customer)
      ...
  }
  ```
  This is the most dangerous one: the comment on the block says it's a security boundary ("Gate here, before the app name ever reaches SQL interpolation"), and the `c.Store != nil` clause silently disables it. With fatal-on-open the security gate runs unconditionally; today a Store-less boot turns the allowlist into a no-op.

- **`coordinator/coordinator.go:493-499`** — tier resolution:
  ```go
  switch {
  case c.Store == nil:
      tier = store.TierBolt
  case custID != 0:
      tier = c.Store.TierForCustomer(custID)
  }
  ```
  The "fall back to most permissive tier so tests whose shards live in the distant past keep querying" comment is the smell. The right fix is for tests to wire a real Store (sqlite/postgres) — production tier resolution should not have a "if no Store, you get unlimited retention" branch.

- **`coordinator/coordinator.go:678-704`** — `runLocal` ShellFS / WeftFuncs / WitFuncs / interior-query rewrite all gated on `c.Store != nil && custID != 0`:
  ```go
  if c.Store != nil && custID != 0 {
      tier := c.Store.TierForCustomer(custID)
      ...
      interp.SetFS(shellfs.New(c.Store, custID, nil, opts...))
      ...
  ```
  `custID != 0` is itself a defensive proxy for "did GetCustomerByName at line 369 silently fail?" — collapses to a single concern once the Store guard is removed.

### 1b. `service.Server.Store` (mirror set in coordinator gRPC handlers)

`cmd/coordinator/main.go:370-374` constructs the gRPC server with `Store: db`. Same root cause: when `db == nil` the Server holds nil, so handlers learned to check.

- **`coordinator/service/server.go:220-243`** — `ListApps` falls back to an empty response when `s.Store == nil`. Dead branch.
- **`coordinator/service/server.go:260-268`** — `SampleCloudWatchSource` skips namespace verification when Store is nil. Same security-degradation smell as coordinator.go:462.
- **`coordinator/service/server.go:300-302`** — `AddSource` returns "config store not available". Dead.
- **`coordinator/service/server.go:353-355`** — `ListSources` returns empty response. Dead.
- **`coordinator/service/server.go:376-378`** — `RemoveSource` returns "config store not available". Dead.
- **`coordinator/service/server.go:403-405`** — `Authenticate` returns "config store not available". Dead.
- **`coordinator/service/server.go:439-441`** — `CreateToken`. Dead.
- **`coordinator/service/server.go:463-465`** — `ListTokens`. Dead.
- **`coordinator/service/server.go:496-498`** — `RevokeToken`. Dead.
- **`coordinator/service/server.go:633-636`** — `authenticateStream` returns "store unavailable". Dead.
- **`coordinator/service/server.go:715-722`** — `GetUsage` lets DB query failure pass and "continue with in-memory only". Borderline (see §3).
- **`coordinator/service/server.go:744-746`** — `resolveCallerCustomerID` returns "config store not available". Dead.
- **`coordinator/service/server.go:921-925`** — `Heartbeat` skips persistence when Store nil. Dead.
- **`coordinator/service/server.go:971-973`** — `authenticateToken` returns "config store not available". Dead.

That's 14 nil checks in one file all sustained by the same boot-time hedge.

### 1c. `api.Server.coordClient` and `api.Server.store`

`cmd/api/main.go:39-42` already fatals on `store.Open` failure (good), and `46-48` fatals on `srv.Connect()` failure (good). So `s.store` and `s.coordClient` are non-nil for the lifetime of every handler. The following are all dead:

- **`api/handlers.go:1041-1044`** — `handleUsage`: `if s.coordClient == nil { ServiceUnavailable }`. Connect already fataled on failure.
- **`api/handlers.go:1261-1264`** — `handleListApps`: same.
- **`api/handlers.go:1273-1276`** — `handleIngestionStatus`: `if s.store == nil { return [] }`. Store can't be nil at this point.
- **`api/shell.go:48-58`** — `if s.store != nil { ... }` and `if s.coordClient == nil { ServiceUnavailable }`. Both dead.
- **`api/sources.go:372-375`**, **`431-434`**, **`493-496`** — three more `s.coordClient == nil` checks. All dead.
- **`api/handlers.go:963-965`** — Stripe webhook handler: `if s.store == nil { break }`. Webhook handler runs in production where store is guaranteed.
- **`api/handlers.go:984-986`** — invoice handler: `if s.store == nil || inv.Parent == nil ...`. The `s.store == nil` clause is dead; the other two are real (Stripe API response shape — out of scope, see §4).
- **`api/handlers.go:1035-1040`** and **`1057-1060`** — `if s.store != nil { ... ActiveSubscription }`. Dead.

### 1d. `api.Server.cfgPtr` / `api.Server.limiter`

- **`api/handlers.go:86-89`** — `currentConfig` falls back to `config.Defaults()` when `cfgPtr == nil`.
- **`api/handlers.go:193-198`** — `Handler()` builds a synthetic atomic.Pointer and limiter when `s.limiter == nil`.

`cmd/api/main.go:98` always calls `srv.SetConfig(...)` before `srv.Handler()`. The fallback path exists only for tests — and tests can call `SetConfig` themselves, or the constructor can install the default. The "tests, dev" comment in `currentConfig` is the only justification, and that's a test ergonomics issue, not a runtime concern.

### 1e. `s.Coord != nil` in service server

- **`coordinator/service/server.go:343`** — `if s.Coord != nil && s.Coord.Provisioner != nil`. The Server is constructed with `Coord: coord` literally one line before `srv.Serve`. Coord is never nil. (Provisioner is always set in main too.)

### 1f. `claims == nil` in HTTP handlers behind `AuthMiddleware`

`api/middleware.go:19-34` is the only writer of `claims` into the request context (via `auth.WithClaims`). It returns 401 on absent or invalid bearer, so any handler reached via the `auth` mux it wraps (`api/handlers.go:295-321`) sees a non-nil `claims`. The following are all dead:

- **`api/handlers.go:465-468`** (`handleResendVerification`)
- **`api/handlers.go:499-502`** (`handleWelcomeDismiss`)
- **`api/handlers.go:652-654`** (`handleMe`)
- **`api/handlers.go:715-717`** (`handleSetupIntent`)
- **`api/handlers.go:825-828`** (`handleCheckout`)
- **`api/handlers.go:1031-1034`** (`handleUsage`)
- **`api/handlers.go:1893-1896`** (`handleDeviceApprove`)
- **`api/sources.go:131-134`** (`handlePendingSources`)
- **`api/sources.go:183-186`** (`handleConfigureSource`)
- **`api/sources.go:281-284`** (`handleListSources`)
- **`api/sources.go:353-356`** (`handleSampleCloudWatchSource`)
- **`api/sources.go:403-406`** (`handleAddCloudWatchSource`)
- **`api/sources.go:477-480`** (`handleDeleteSource`)

13 occurrences. Note that several other handlers in the same file already trust the middleware and just write `claims := GetClaims(r); ... claims.UserID` (e.g., `handleListQueries:1090-1091`, `handleCreateQuery:1100`, `handleListTokens:1136-1137`) — that pattern is the right one and shows the inconsistency.

### 1g. `agent == nil` in agent-token handlers behind `AgentTokenMiddleware`

`api/middleware.go:92-113` returns 401 on missing/invalid token and only stores a non-nil `*AgentAuthResult` on success. Behind `AgentTokenMiddleware` (mounted at `/api/agent/`):

- **`api/sources.go:27-31`** (`handleAgentSample`)
- **`api/sources.go:75-79`** (`handleAgentSourceStatus`)

Both dead.

---

## 2. Map / slice nil-vs-empty conflation

Searched for `if m != nil { for _, v := range m }` and the `if X != nil { for ... }` shape. None of significance turned up — the codebase mostly trusts Go's nil-range semantics. The closest case is the `if c.SeenApps != nil` guard at **`coordinator/coordinator.go:402`** and **`coordinator/service/server.go:213`**: `SeenApps` is set unconditionally in `NewCoordinator` (`coordinator.go:140`), so the guard is dead — but the failure mode is benign (range over nil map is fine), so this is mostly noise that can come out without behaviour change.

---

## 3. Error swallowing to fall back to permissive behavior

- **`coordinator/coordinator.go:351`** — `if cust, err := c.Store.GetCustomerByID(watch.CustomerID); err == nil`. The watch came from the Store one stack frame up; `GetCustomerByID(watch.CustomerID)` can only fail on a real DB error or a deleted-customer race. Silently dropping the customer name turns the next /bin pre-expand into a no-op. Should be `log.Printf` + return — a watch whose owning customer is gone shouldn't keep firing.

- **`coordinator/coordinator.go:370`** — `if cust, err := c.Store.GetCustomerByName(customer); err == nil`. Same shape. The `customer` here is the JWT's authoritative customer name (line 56 in `service/server.go` resolves it from claims, rejecting empty). A miss here means the JWT references a customer that doesn't exist in the DB — that's an auth-layer failure, not "run with `custID = 0` and degrade silently."

- **`coordinator/coordinator.go:404`** — `if expanded, err := planner.ExpandSourceGlobs(pipeline, apps); err != nil { return err }`. Real error handling, no swallow — kept for contrast.

- **`coordinator/service/server.go:715-722`** — `s.Store.QueryUsage` failure logged + "Continue with in-memory only." Returning a sum that silently omits the persisted half lies to the dashboard. Should propagate.

- **`coordinator/service/server.go:225-241`** — `ListApps` swallows `GetCustomerByName`, `ListNamespaces`, and `AllAppNames` errors and returns `&ListAppsResponse{}` on each. Three swallows in one method; any one of them should be a 500 or at least a logged + returned status.

- **`api/handlers.go:1733-1740`** — `cust, _ := s.store.GetCustomerByName(member.CustomerName); if cust == nil { 500 }; user, _ := s.store.GetUserByEmailAndCustomer(...)`. The `cust == nil` check after `_` swallow is the standard "use nil-return as the failure signal" pattern but loses the underlying error.

- **`coordinator/alert.go:134`** — `results, _ := ctx.Store.ListCronResults(ctx.Watch.ID, 2, r.tag)`. A real DB error during alert evaluation will silently make `hasPrev = false` and skip rising/falling/changed evaluation. Should at least log.

- **`coordinator/alert.go:340`** — `existing, _ := s.OpenIncident(watchID, condition, tag)`. A swallowed error here causes spurious incident creation on every tick. Worth logging.

---

## 4. Re-validation across trust boundaries the boundary already enforced

- **`coordinator/service/server.go:99-103`** — `Query` calls `callerCustomer(stream.Context())` which reads the JWT-derived `auth.FromContext`. The auth interceptor (registered in `cmd/coordinator/main.go:367-368`) already validated the token and would have returned `Unauthenticated` for any RPC that didn't carry one (`Query` is NOT in the `SkipMethods` set). The `if c == nil` and `if c.CustomerName == ""` guards in `callerCustomer` (`server.go:55-63`) duplicate the interceptor's gate.

- The **§1f** cluster (13 handlers checking `claims == nil` after `AuthMiddleware`) is the textbook case of this category, listed there because the guards are also dead-dependency checks.

- **`coordinator/coordinator.go:462-466`** — `executeQuery` re-fetches the customer by name to allowlist the source app. The `customer` argument was already resolved at line 369 via `GetCustomerByName(customer)`; refetching the same row two stack frames apart is wasteful but defensible — the second fetch's purpose is the `CustomerHasSource` check, which needs the ID. Borderline (see §5).

---

## 5. Default-zero-value defenses

- **`store/tmp.go:456`**, **`store/tmp.go:661`** — `if f.LineCount == 0` to skip empty file handling. Real domain check, not defensive.

- **`api/handlers.go:1023`** — `if rate == 0 { return 0 }` for overage calculation. Real (a tier with no overage price returns rate 0).

- **`api/handlers.go:1449`** — `if maxWatches == 0 { ... }`. Real tier-config branch.

Nothing surgical here. The category is mostly clean.

---

## 6. Unreachable error branches with "shouldn't happen" comments

`grep` for `// shouldn't happen`, `// can't happen`, `// impossible`, `// should not happen` returned no hits. Either the codebase doesn't write those comments, or it inlines the check without explanation. Manual sweep didn't surface any.

---

## 7. Defensive type assertions

Searched `x.(*T); ok` and `x.(string); ok` patterns. All the hits are legitimate dynamic dispatch:

- **`coordinator/launcher.go:65`** — `l.(asyncLauncher)` — capability probe.
- **`coordinator/provisioner.go:693`** — `p.launcher.(RecoveryTerminator)` — capability probe.
- **`coordinator/bin_expand.go:141`** — `fs.(bash.LangFS)` — capability probe.
- **`store/shell.go:438`** — `n.(*bash.SimpleCommand)` — AST node walk.
- **`shell/http_remote.go:174-361`** — `event["data"].(map[string]any)` and friends — JSON `any` decoding.

Category empty.

---

## 8. Backward-compat / legacy shims pre-launch

Per `feedback_no_legacy_support.md`: rip and replace, no compat shims. Hits:

- **`cmd/coordinator/main.go:37`** — `--shard-asg` flag described as "(legacy single-tier)" with fallback at line 137 (`shardTiers["small"] = *shardASG`). If the tiered ASGs are the production shape (and they are — see deploy infra), the legacy single-ASG flag is dead.

- **`coordinator/coordinator.go:124, 265`** — `// Shards are registered with empty Apps (legacy: matches all apps).` and the matching code in `registry.go:29` (`Apps []string // empty = all apps (legacy/shared)`). If this is pre-launch, just require non-empty Apps and rip the empty-means-all branch out of `MatchingShards`.

- **`coordinator/coordinator.go:479, 555`** — comments call `ShardSQL` "the legacy ts-ordered SQL" kept for "callers still using the legacy Query RPC (no compute stages, backward-compat tests)". Two parallel SQLs (`ShardSQL` + `DirectShardSQL`) doubles the planner surface; if the no-compute path is still the live one for non-streaming queries, drop the "legacy" framing — if it's actually dead, delete the path.

- **`coordinator/service/server.go:450`** — `// App field used as namespace name for backward compat with gRPC callers.` Pre-launch: rename the proto field, regenerate, done.

- **`coordinator/service/server.go:480-491`** — `ListTokens` populates `TokenInfo.Token` with `t.Prefix` because "Dead path — no callers; retained for proto-compat until we regen with an id+prefix schema." Self-documented dead code.

- **`coordinator/service/server.go:945-953`** — `authenticateHeartbeat`: returns `nil` for unknown shard IDs and pre-015-migration shards. Comment: "future tightening can reject here once we've confirmed no legitimate flow relies on an unknown id." Pre-launch is when that confirmation happens; this is a security-sensitive grandfather clause that should not survive to launch.

- **`store/token.go:97`** — `// AgentAppNames is a backward compat alias.`
- **`store/token.go:139`** — `// Only used by the legacy coordinator gRPC` for `RevokeAgentTokenByRawToken`. Confirm and delete if true.

- **`planner/parse.go:227`** — `"/log/vimgym.log" → "vimgym" (flat namespace, backward compat)`. If the flat-namespace shape was ever the public surface, fine; if it was a transitional name, rip.

---

## Borderline cases

- **`coordinator/coordinator.go:493-499`** — tier-resolution falling back to `TierBolt` "for tests." Real concern is test ergonomics; the question is whether tests should pass a real (sqlite-backed) Store or a stub. If the answer is "tests use a stub Store that's nil," the production code shouldn't carry the price.

- **`coordinator/service/server.go:715-722`** — `GetUsage` "Continue with in-memory only" on Store error. Defensible (better to show partial than 5xx the dashboard) but should at minimum return a flag in the response so the UI can render "incomplete." Today it silently lies.

- **`coordinator/coordinator.go:462`** — second `GetCustomerByName` lookup in `executeQuery`. Refetching for `CustomerHasSource` is fine; the question is whether `GetCustomerByName` should return both customer and source-allowed flag in one round-trip. Leave alone.

- **`api/middleware.go:43-46`** in `RequireEmailVerified` — checks `claims == nil` after the chain has already gone through `AuthMiddleware`. The middleware chain order in `Handler()` is `AuthMiddleware(jwtSecret, auth)` wrapping a mux that includes `protected(...)` handlers, where `protected = RequireEmailVerified(...)`. So `RequireEmailVerified` runs after `AuthMiddleware`; the nil check is dead. But this is a generic middleware that *could* be reordered, so the check is cheap insurance. Leave it as documentation, or rip it and add a comment that the chain order is load-bearing — pick one.

- **`coordinator/alert.go`** AlertContext nil guards (`ctx.Watch != nil && ctx.Store != nil`) — the AlertContext is constructed in two spots: `coordinator.go:338` (always sets Store + Watch from non-nil inputs in `QueryForWatch`) and `coordinator.go:426` (Interactive=true, Watch is nil intentionally). So `ctx.Watch != nil` is a real branch (interactive vs cron). `ctx.Store != nil` rides along on the dead-Store cascade and would simplify to "always true" once §1a is fixed.

- **`coordinator/coordinator.go:618`** — `if errOut.Len() == 0 { return }` is a real "no stderr produced" optimization, not defensive.

- **`coordinator/notify.go` lines that swallow JSON marshal errors** — `payload, _ := json.Marshal(...)` for a fixed-shape map literal. Marshal errors on `map[string]string` are genuinely impossible; the `_` is honest about that. Leave alone.

---

## Out of scope (real defenses)

- `store/*.go` parameter validation against user-supplied input (path traversal in tmp.go, name validation in shell.go). Real boundary.
- `auth.Validate` (jwt.go:81+) — checks token shape, signature, expiry on attacker-controlled input. Real.
- `planner/parse.go` — pipeline parsing on user-typed input. Real.
- `ingest.Detect` schema sniffing — log content from upstream. Real.
- AWS SDK error returns (ASG operations, S3 ops, EC2 calls in launcher_*.go and provisioner.go). Real external boundaries.
- Stripe webhook payload shape checks (`inv.Parent != nil`, `inv.Parent.SubscriptionDetails != nil`). Real — Stripe response shape varies by event.
- gRPC `stream.Recv()` error handling. Real network boundary.
- File I/O error returns in `shellfs/`, `shard/`, etc. Real.
- DuckDB query errors from shard nodes. Real.
- Heartbeat token validation (`authenticateHeartbeat:955-963`). Real, attacker-controlled.
- The `if !triggered { ... } return 0` short-circuit in `evaluateAlertRow` (alert.go:268). Real domain logic.

---

## The one root-cause fix

Change `cmd/coordinator/main.go:112-117` from:

```go
if d, err := store.Open(dsn); err != nil {
    log.Printf("warning: config store unavailable: %v", err)
} else {
    db = d
    defer db.Close()
}
```

to a `log.Fatalf` (mirroring `cmd/api/main.go:39-42` which already does this). That single change makes every nil check listed in §1a, §1b, §1c, §1d, §1e, §1f, §1g unreachable in production, and most of §3 either becomes a real error or moves to §2-style noise. Then strip them.
