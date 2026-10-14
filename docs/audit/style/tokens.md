# Style review: docs/manual/tokens.md
Verified: ✅ 8 · ❌ 4 · ⚠️ 4 · 🔒 6 · 🖥 4 · ∅ 1   (27 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~230

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes — "Create a token" (l.10).
- [ ] 2. Task-oriented sentence-case headings — fine, but path layout lives under "Binding env to a token" (l.23-46); split it out.
- [ ] 3. Every code block copy-pasteable — l.34 ok with own namespace; paths at l.40-44 invalid (tok-18/20/21).
- [ ] 4. Realistic names — `/log/myco/api/main.log` 3-seg is not what env-bound tokens produce; heartbeats/runtime logs absent.
- [x] 5. Commands/flags in `code`, UI bold — consistent.
- [?] 6. Sample output matches — CLI prints `Token created: wft_...`; doc shows none; web dialog unchecked.
- [x] 7. No internal vocabulary — l.27 "before it reaches a shard" leaks "shard"; change to "before it is stored".
- [?] 8. Terminology — "env"/"deployment environment" vs `DEPLOYMENT` meta-var; "namespace" ok.
- [x] 9. No stale roadmap language; no Windows — none.
- [ ] 10. Voice — l.38-39 "no path contortions required" informal; l.37-46 paragraph overlong.
- [?] 11. Cross-references — **Query your logs** (l.45) is a title; confirm it exists in both targets.
- [x] 12. Renders cleanly — static check clean.
- [?] 13. Length proportionate — short, but missing list/revoke; path layout may duplicate **Namespaces**/**Query your logs**.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| tok-02 | 12 | "Each token is scoped to a single namespace" | decide (see below); else "one or more namespaces (comma-separated)" |
| tok-18/19 | 39-42 | plain apps land at `/log/myco/api/main.log` | "Rows from an env-bound token land at `/log/myco/<env>/<service>/<kind>.log`; a token with no env lands at `/log/myco/<service>/<kind>.log`." (no-env case unverified) |
| tok-20/21 | 42 | `heartbeats.log`, `runtime.log` | "when the agent emits them" |
| tok-23 | 43-44 | "a token spanning multiple envs" uses 4-seg form | any env-bound token yields the 4-segment form |
| tok-05/27 | 15, doc end | shown once, no recovery | add `weft token list`, `weft token revoke --id ID`; "if you lose it, revoke and create a new one" (draft) |
| tok-17 | 37-38 | "any query the token authenticates implicitly filters to that env" | ambiguous: tokens authenticate agents (ingest); clarify whether tokens can query |
| style | 27 | "reaches a shard" | "is stored" (draft) |
| style | 38-39 | "no path contortions required" | drop |

## Needs your decision first (product vs doc)
- Single namespace per token (tok-02): `--namespace acme,globex` created a 2-namespace token. Validate to one namespace (doc correct), or document comma lists.
- Heartbeats/runtime telemetry paths (tok-20/21): confirm they exist in real deployments, or remove from doc.
- No-env token layout (tok-18): confirm 3-segment layout still exists for tokens without env; if not, doc should say 4-segment always.

## Notes
- Agent-side claims (tok-01/07/09/10/16) need a host with weftd; only CLI behavior was verified.
- Move the path-layout paragraph (l.37-46) to **Query your logs** or a namespaces doc; it is not about creating tokens.
- Web UI claims (Namespaces page, **Create token**, Env field; tok-03/04/11) need a browser check.
- Sibling doc connect-heroku shows `issued: wft_...`; CLI prints `Token created:` (that doc is stale, not this one).

## Applied 2026-10-12
- One EXISTING namespace, 404 text, invalid-name rules, `--namespace a,b` not multi (decision 7); button **new token**; list/revoke recovery; "shard" -> "stored"; path paragraph rewritten (4-seg for env-bound, 3-seg env-less, 3-seg reads across envs); dropped "implicitly filters" and "contortions". Left: web Env field not browser-checked.
