# Style review: docs/manual/namespaces.md
Verified: ✅ 13 · ❌ 3 · ⚠️ 9 · 🔒 18 · 🖥 0 · ∅ 2   (45 claims)
Audience: customer-facing web+shell   Words: ~400

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes ("Create a namespace")
- [ ] 2. Task-oriented sentence-case headings, no filler
  - L23 "Log path layout" is reference, not a task; no H1 above "Create a namespace" (namespaces-44).
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L20 `namespace create production` fails in the shell (namespaces-09/10).
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
  - L15 **Create namespace** is not the real button label (`new`, namespaces-04).
- [x] 6. Sample output matches the verified output (no sample output)
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch); Render `cron` kind is legitimate
- [x] 9. No stale roadmap language; no Windows
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L61-62 "as different tokens reauth over time" is vague; L16 "token identifiers and CLI scoping" is loose.
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - L63-64 "**Meta-variables**" has no `man metavars` hint; no link.
- [x] 12. Renders cleanly in both targets
- [?] 13. Length proportionate; no duplicate of another doc
  - Path-layout rules may overlap query-logs; mint command never shown here.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| namespaces-09/10 | 18-21 | `# From the shell` / `namespace create production` | "# From your terminal" / `weft namespace create --name production` |
| namespaces-04 | 15 | click **Create namespace** | click **new** (confirm in browser) |
| namespaces-40 | 61-62 | "side by side as different tokens reauth over time" | "A namespace can hold both env-less and env-bearing streams, depending on how each token was minted." |
| namespaces-07/08 | 16 | "it becomes part of token identifiers and CLI scoping" | "it is used to scope tokens, sources, and log paths." |
| namespaces-19 | 40 | "minted with `--env`" (mint command never shown) | add `weft token create --namespace NS --env ENV` example (draft) |

## Needs your decision first (product vs doc)
- namespaces-09: `namespace create` is not a shell builtin. Add it to the shell, or document `weft namespace create --name` (ledger proposes the latter).
- namespaces-25: `heartbeats` and `runtime` kinds never observed in dev data. Keep if weftd/SDK emits them; otherwise trim the list.
- namespaces-14/21: env-less 3-segment layout could not be observed (no env-less token). Confirm it exists before keeping both layouts.

## Notes
- Lowercase naming rule (L16) is unverified (namespaces-05/06); `ls /log/Acme` shows paths are case-sensitive.
- `/log/<ns>/<env>.log` is rejected ("missing a kind segment"); the doc could mention it next to the wildcard rule.
- Section order: "Where the service name comes from" table is the most reusable part; Heroku/Render/CloudWatch docs link to it.

## Applied 2026-10-12
- Button **new**; `weft namespace create --name production`; name rules (no comma/whitespace//+*?[, no leading -); scoping wording; env-less/env-bearing sentence; glob refusal and `<ns>.log` (decisions 2, 7). Removed "reader token never has to guess". Left: heartbeats/runtime kinds list.
