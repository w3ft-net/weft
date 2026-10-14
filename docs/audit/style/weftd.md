# Style review: docs/manual/weftd.md
Verified: ✅ 11 · ❌ 3 · ⚠️ 20 · 🔒 80 · 🖥 2 · ∅ 7   (123 claims)
Audience: customer-facing web+shell   Words: ~1830

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes ("Install weftd")
- [ ] 2. Task-oriented sentence-case headings, no filler
  - L247 "App API: emit events and heartbeats from your services" and L112 "Schema types" are reference; doc is long, mixes install, metrics, SDK.
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L17 install.sh returns HTML (weftd-05); L38, 207, 231-239, 289, 317 use 3-segment paths; L270-278 and L296-301 mix shell and Go in one block.
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
  - Block language tags inconsistent (L140 `yaml`, L291 `json`, rest untagged).
- [ ] 6. Sample output matches the verified output
  - L219 "one row per `/dev/*` mount" shows `/etc/hosts` etc. in containers (weftd-85).
- [ ] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
  - L45 `.shard_id` leaks "shard"; L45 `/etc/weft/identity.json`; L210 undefined "ASG".
- [ ] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
  - L309-310, L316 "watch" (weftd-116/118); `<ns>` vs `<namespace>` varies.
- [?] 9. No stale roadmap language; no Windows
  - L167-169 "first restart ... still loses lines" is a version-bound caveat that goes stale; L243-245 Linux-only is fine.
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L85-86 "an AWS-shaped credential chain" is odd; L325 "Three-line setup" has four statements (weftd-121).
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - L90-93 points to **Meta-variables** in **Query your logs**; metavars doc spells `version.file:` vs this doc `version_file:` (weftd-34).
- [x] 12. Renders cleanly in both targets (generated install-weftd.ts escapes correctly)
- [ ] 13. Length proportionate; no duplicate of another doc
  - 338 lines; checkpointing, metrics, and App API/SDK could be their own docs. Service-name rules duplicate **Namespaces** table.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| weftd-13/79 | 38, 207 | `/log/<ns>/<service>/metrics.log` | `/log/<ns>/<env>/<service>/metrics.log` |
| weftd-91/92 | 231-239 | `/log/myns/api/metrics.log` | `/log/myns/prod/api/metrics.log` |
| weftd-109 | 289, 317 | `/log/<namespace>/<service>/heartbeats.log`, `/log/myns/billing/heartbeats.log` | add `<env>` segment |
| weftd-103 | 271 | `billing:user signup id=42` -> `/log/<ns>/billing/...` | note 4-segment path in result |
| weftd-116 | 309 | "which a watch expresses directly" | "which a cron expresses directly" |
| weftd-118 | 316-317 | "where billing-beat is a watch like:" | "where billing-beat is a cron (`/cron.d/billing-beat`) whose body is:" and add `# schedule: */1 * * * *` first line |
| weftd-121 | 325 | "Three-line setup" | "Four-line setup" or drop the count |
| weftd-34 | 54 | `version_file:` | align with metavars (`version.file:` / `version.env:`); one doc is stale |
| weftd-05 | 17 | `curl -fsSL https://w3ft.net/install.sh \| sh` | host install.sh, else point to **Downloads** page / `dpkg` only |

## Needs your decision first (product vs doc)
- weftd-05: install.sh serves the SPA HTML. Deploy the script, or remove the one-liner from the doc.
- weftd-34: config key `version_file:` (this doc) vs `version.file:`/`version.env:` (metavars). Which is real; fix the stale doc or rename the key.
- weftd-117: `weft::is_absent` on a nonexistent cron returns false, so an uncreated heartbeat cron never alerts. Document the requirement, or make unknown crons absent.
- weftd-76: default `interval` 30s vs 10s observed in dev; confirm dev override, else fix doc.

## Notes
- 80 claims need a host and weftd (🔒); none of install, checkpointing, metrics fields, or socket behavior was black-box tested.
- Consider splitting: Install, Checkpointing, System metrics, App API/SDK.
- L28-29 comment says "defaults to this"; fine, but L137-138 `StateDirectory=weftd` is systemd internals.
- L210 define or drop "ASG"; L45 replace `.shard_id` with a neutral example (e.g. `.node_id`).

## Applied 2026-10-12
- `version_file:` -> `version: {file:, env:}` (flat key is rejected by weftd); heartbeat section: watch -> cron, real `# schedule:` headers, no -n/tail counts, alert via `alert --if-zero "$?"`; "Three-line setup" -> "Setup".
- Left: install.sh one-liner (serves HTML), `schema:`/`prefix_fields` source keys (weftd config has no such keys; schema is server-side, so the Schema types section looks stale), query-logs.md still says `version_file` (other doc), default metrics interval, ASG/`.shard_id` wording.
