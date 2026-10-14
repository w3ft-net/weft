# Ledger: docs/manual/schema-setup.md
| id | line | claim | type | black-box test | verdict | evidence |
|----|------|-------|------|----------------|---|---|
| schema-01 | 12-13 | Once weftd ships logs, weft auto-detects schema from the first batch of lines | default | needs-human (install weftd on a host, send logs, observe detection) | 🔒 | Needs weftd on a host: install weftd, point at a fresh log file, send a first batch, then open Namespaces. Not runnable read-only. |
| schema-02 | 13 | A **Namespaces** page exists in the web UI | UI | log in to web UI, click Namespaces | 🖥 | Log in to the web UI at the dev endpoint and look for Namespaces in the nav. |
| schema-03 | 13-14 | Namespaces shows detected apps and their field schemas | UI | open Namespaces after ingest, inspect app list and field schema | 🖥 | After ingest, open Namespaces and check the app list shows each app with its field schema. |
| schema-04 | 16-17 | A **pending source** notification appears on the dashboard when data arrived but schema is unconfirmed | UI | needs-human (requires fresh weftd source); view dashboard | 🖥 | Needs a fresh weftd source; open the dashboard and look for the "pending source" notification. |
| schema-05 | 17-18 | Notification means weft received data but needs you to confirm detected schema | UI | needs-human | 🖥 | Click the pending source notification and read its text. |
| schema-06 | 18-19 | You can review sample lines and approve | UI | click notification; confirm sample lines + approve control | 🖥 | Click the notification; confirm sample lines are shown and an approve control exists. |
| schema-07 | 20-21 | Two fields come from parsing each log line per source's schema | default | `weft.run`/shell: `cat /log/<ns>/<svc>/main.log \| awk '{print TS, LEVEL}'` | ✅ | `tail -n 3 /log/acme/prod/api/main.log | awk '{print TS, LEVEL}'` -> `2026-10-12 12:12:33.922 INFO` x3. Both fields populate. (Note TS prints as space-separated, not the raw ISO-Z in the line.) |
| schema-08 | 22 | `TS` is the timestamp field | default | `cat /log/... \| awk '{print TS}'` | ✅ | Same command: TS = `2026-10-12 12:12:33.922`, matches raw line timestamp `2026-10-12T12:12:33.922Z`. |
| schema-09 | 23 | `LEVEL` is the log level field with values ERROR, WARN, INFO, DEBUG | default | `cat /log/... \| awk '{print LEVEL}' \| sort \| uniq -c` | ⚠️ | `awk '{print LEVEL}' | sort | uniq -c` over 1000 lines each of prod api/worker/edge main.log and sand api: only ERROR, WARN, INFO seen (e.g. 99 ERROR/745 INFO/156 WARN). DEBUG never observed (not proof it is absent; dev data may simply lack it). Cannot confirm the DEBUG value. |
| schema-10 | 25-26 | Other indexed fields: SERVICE, HOST, DEPLOYMENT, ENV, VERSION, REGION | default | print each in awk against a real log | ⚠️ | `tail -n 2 /log/acme/prod/api/main.log | awk '{print TS"|"SERVICE"|"HOST"|"DEPLOYMENT"|"ENV"|"VERSION"|"REGION...}'` -> `...|api|7e4ae977dce2|api|prod||||||`. SERVICE, HOST, DEPLOYMENT, ENV populate; VERSION and REGION are empty on this dev data (NULL when untagged, per metavars). All names are valid. Also the doc calls TS/LEVEL non-indexed while the metavars table lists them as columns. |
| schema-11 | 26 | There are "four k8s-mode dimensions" (unnamed) | number | check meta-variable table in `man metavars` lists four k8s fields | ✅ | Static: docs/manual/metavars.md table has exactly four k8s-mode rows: CLUSTER, K8S_NAMESPACE, POD, CONTAINER. Runtime: all four evaluate (empty on this non-k8s data) with no error. Caveat: the table also has NODE (weftd dimensions.node), which the doc list omits. |
| schema-12 | 26-27 | These are stamped by weftd, the agent token, or cloud metadata, not parsed from the line | default | unverifiable black-box (internals); partly: send line containing fake `ENV=x`, confirm ENV unchanged | ⚠️ | Static: metavars Source column agrees (HOST=weftd hostname, ENV=agent token, REGION=cloud metadata, DEPLOYMENT/VERSION=weftd config). Spoof test (send line with ENV=x) needs ingest, 🔒. Dev rows show ENV=prod from the token/path, not the line text. Also LEVEL/TS are listed there as "schema-extracted", consistent with the doc. |
| schema-13 | 27-29 | **Meta-variables** section exists in **Query your logs** with full table and sourcing | xref | open Query your logs manual; `man metavars` | ⚠️ | Doc says Meta-variables section is in "Query your logs": docs/manual/query-logs.md line 36 has `## Meta-variables`, but the full table with sourcing lives in the separate topic `man metavars` (docs/manual/metavars.md, "Reserved set"). Query-logs section is a summary. Pointer works only if that section links on; reader may not find the "full table". Static only; `man` is 🖥. |
| schema-14 | 4-7 | Frontmatter: section schema-setup, category connect-source, order 45 | UI | web manual placement | 🖥 | Static: frontmatter present (section schema-setup, category connect-source, order 45); generated frontend/src/generated/docs/schema-setup.ts and `man schema-setup` entry in shell/help_corpus_gen.go exist with no stray `{{`/`&lt;` in schema-setup.ts. Web manual placement needs a browser. |

## Style observations
- "weftd" described only as agent; fine. No warp, Windows, "coming soon", or watch terms.
- Line 26 "four k8s-mode dimensions" is unnamed and vague; cross-reference is to a section by bold title rather than doc id.
- The doc says TS/LEVEL are "indexed fields" implicitly only in the next paragraph ("Other indexed fields"); inconsistent with "Two fields come from parsing" (are TS/LEVEL indexed?).
- Doc is very short; no steps to verify, no sample output, no mention of how to reject/edit a detected schema.

## Style observations: results
- "Two fields come from parsing" vs "Other indexed fields": confirmed ambiguous; metavars lists TS and LEVEL as columns too. Unnamed "four k8s-mode dimensions" does map to CLUSTER, K8S_NAMESPACE, POD, CONTAINER.
- No watch, Windows, warp or "coming soon" terms (grep of doc: none).

## Findings
- schema-09 (doc line 23): lists DEBUG; no DEBUG rows observed in 5 streams x 1000 rows. Probably fine, undetermined; consider "(ERROR, WARN, INFO, ...)" only if DEBUG is not actually extracted. Doc may be OK.
- schema-10 (lines 25-26): VERSION/REGION are empty unless weftd is configured for them. Proposed: "...`VERSION`, `REGION` (empty when not configured)".
- schema-11 (line 26): omits NODE. Proposed: name them: "`CLUSTER`, `K8S_NAMESPACE`, `POD`, `CONTAINER` (k8s mode), and `NODE`".
- schema-13 (lines 27-29): full table is in `man metavars` (own topic), not in the Query your logs section. Proposed: "see **Meta-variables** (`man metavars`) for the full table and how each is sourced." Doc stale/ambiguous.
- schema-12 / TS-LEVEL "indexed" wording: ambiguous (see style). Proposed: "Other fields (...)" instead of "Other indexed fields".
- Doc gives no way to reject or edit a detected schema (omission, unverified in UI).

