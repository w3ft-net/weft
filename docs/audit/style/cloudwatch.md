# Style review: docs/manual/cloudwatch.md
Verified: ✅ 3 · ❌ 0 · ⚠️ 12 · 🔒 22 · 🖥 7 · ∅ 2   (46 claims)
Audience: customer-facing web+shell   Words: ~640

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes
- [x] 2. Task-oriented sentence-case headings, no filler
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L93 path omits env segment (cw-35); L91-94 comment and command mixed; L60 placeholder needs token (cw-23).
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [x] 6. Sample output matches the verified output (no sample output)
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
  - L65 "namespace-scoped agent token" vs "namespace token" at L60 and "producer token" elsewhere (cw-23).
- [?] 9. No stale roadmap language; no Windows
  - L77 "Once enough lines have accumulated" has no threshold (cw-28); L12 "Kinesis Data Firehose" renamed Amazon Data Firehose.
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L76 "nothing to pull on demand" contradicts L110 "pull a fresh sample".
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - **Namespaces** by title; L23-24 "install weftd" should name **Install weftd** (cw-06, 🖥).
- [x] 12. Renders cleanly in both targets
- [?] 13. Length proportionate; no duplicate of another doc
  - Schema wizard overlaps weftd/schema-setup content; "pending" source status also covered in schema-setup.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| cw-45/27 | 110-111 | "`weft source resample` to pull a fresh sample and re-run the wizard" | "then `weft source resample <namespace>/<service>` to discard the current sample, let a fresh one accumulate, and re-run the wizard" (only if behavior matches) |
| cw-08/35 | 29-30, 93 | `/log/myco/my-app/main.log` | `/log/<namespace>/<env>/<service>/main.log` (or without `<env>` for an env-less token; see **Namespaces**) |
| cw-13 | 43 | "The source shows up as pending" | add "(or run `weft source pending`)" |
| cw-23 | 65 | "namespace-scoped agent token" | "namespace token" (draft; matches L60 placeholder) |
| cw-29/33 | 80, 88 | "no local file to manage" | note optional `--file schema.yaml` (usage shows `[--file schema.yaml]`) |
| cw-41 | 107 | `weft source schema` "shows accumulation progress" | unverified: configured source prints schema only |

## Needs your decision first (product vs doc)
- cw-45: does `resample` pull from CloudWatch or reset accumulation? Fix the doc to match once a human runs it, or rename product verb.
- cw-41: if `source schema` shows no progress for unconfigured sources, either add progress output or drop the sentence.
- cw-08: where CloudWatch sources land (with or without env) is unconfirmed; depends on token env binding.

## Notes
- 22 claims are AWS-side (CFN template, Firehose, 2-filter limit) and need a human with an AWS account.
- L59 template URL has no version pin; consider whether that is intended.
- L91-94 block mixes a `# And in the shell` comment with a bare command; split or use the `weft>` prompt consistently.
- `weft source ... --help` is not a real flag (any arg prints usage); avoid suggesting it.

## Applied 2026-10-12
- resample wording matches CLI ("collect a fresh sample ... re-run once it completes"); `weft source pending` mentioned; "agent token" -> namespace token for an existing namespace; note that the 3-segment path spans envs and env tokens use the 4-segment form.
- Left: all AWS-side claims, template URL, "Kinesis Data Firehose" naming, `--file schema.yaml` omission.
