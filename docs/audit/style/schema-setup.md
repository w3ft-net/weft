# Style review: docs/manual/schema-setup.md
Verified: ✅ 3 · ❌ 0 · ⚠️ 4 · 🔒 1 · 🖥 6 · ∅ 0   (14 claims)
Audience: customer-facing web+shell   Words: ~150

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background
  - L12: opens with "weft auto-detects" (background); lead with "Confirm the detected schema".
- [x] 2. Task-oriented sentence-case headings, no filler
- [x] 3. Every code block is copy-pasteable and runs as written (no code blocks; none needed, but see note 1)
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [x] 6. Sample output matches the verified output (no sample output)
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L16-18 "it means ... Review ... and approve" is passive-ish and vague; no approve control named (schema-06 🖥).
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - L27-29 points to section in "Query your logs"; full table is in `man metavars` (schema-13).
- [x] 12. Renders cleanly in both targets (schema-14 static check clean)
- [?] 13. Length proportionate; no duplicate of another doc
  - Very short; no verify step or sample output. May overlap metavars doc table.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| schema-13 | 27-29 | "see the **Meta-variables** section in **Query your logs** for the full table" | "see **Meta-variables** (`man metavars`) for the full table and how each is sourced." |
| schema-12 | 25 | "Other indexed fields (...)" | "Other fields (...)" (TS/LEVEL are also columns, so "indexed" is ambiguous) |
| schema-11 | 26 | "the four k8s-mode dimensions" | "`CLUSTER`, `K8S_NAMESPACE`, `POD`, `CONTAINER` (k8s mode), and `NODE`" |
| schema-10 | 25-26 | "`VERSION`, `REGION`" | "`VERSION`, `REGION` (empty when not configured)" |

## Needs your decision first (product vs doc)
- schema-09 (L23): `LEVEL` lists DEBUG but no DEBUG rows were seen in 5 streams. Keep if extractor emits it; else drop DEBUG from the list.
- Reject/edit a detected schema: doc is silent. Document the flow if the UI supports it, or say approval is the only action.

## Notes
- No sample command to verify the result; schema-07 shows `awk '{print TS, LEVEL}'` works on a real path and would make a good "confirm it worked" step (prints TS space-separated, not ISO-Z).
- The **Namespaces** and **pending source** UI paths are all unverified in a browser (schema-02..06, 🖥).

## Applied 2026-10-12
- Meta-variables pointer now `man metavars`; "indexed" dropped; k8s dimensions and NODE named; REGION/VERSION empty-when-unset note.
- Left: DEBUG in LEVEL list, reject/edit flow, UI claims.
