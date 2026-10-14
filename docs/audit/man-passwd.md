# Ledger: docs/shell/man-passwd.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| man-passwd-01 | 7-10 | Reachable by `man passwd`, `man adduser`, `man deluser`, `man groups` | command | Run all four | ✅ | static: help_corpus_gen.go lines 117-120 map man passwd/adduser/deluser/groups to ManPasswd. Live `man` is 🖥 (command not found under sh -c); type `man passwd` etc. in `weft sh`. |
| man-passwd-02 | 11 | Banner is `PASSWD(5)` | output | `man passwd` | ✅ | static: ManPasswd const begins `PASSWD(5)   Weft Commands   PASSWD(5)`. Live render 🖥. |
| man-passwd-03 | 16 | passwd = org team roster and group-based permissions | default | `cat /etc/passwd` | ✅ | `cat /etc/passwd` (as alice, acme) returns a roster of members with role/status/groups. |
| man-passwd-04 | 21 | `cat /etc/passwd` valid | command | Run | ✅ | `cat /etc/passwd` prints the roster table. |
| man-passwd-05 | 22 | `adduser <email> [<group>]` syntax | command | `adduser x@y.com` and with group | 🔒 | Mutation. Human: `adduser x@y.com` then `adduser x@y.com payments`. (Not run, forbidden.) |
| man-passwd-06 | 23 | `deluser <email> [<group>]` syntax | command | Run both forms | 🔒 | Mutation. Human: `deluser x@y.com payments`, then `deluser x@y.com`. |
| man-passwd-07 | 24 | `groups [<email>]` syntax | command | `groups` and `groups x@y.com` | ✅ | `groups` -> `alice@acme.test: binmgr cronmgr ...`; `groups bob@acme.test` -> `bob@acme.test: binmgr cronmgr configmgr nsmgr acme acme+tokens`. Unknown email -> `no such member: ...`. |
| man-passwd-08 | 25 | `id [<email>]` syntax | command | `id` and `id x@y.com` | ✅ | `id` -> `uid=alice@acme.test role=admin groups=binmgr,cronmgr,...`; `id carol@acme.test` -> `uid=carol@acme.test role=admin groups=binmgr,cronmgr,configmgr,nsmgr,acme`. Note output format is `uid=.. role=.. groups=a,b` (comma separated), not described in doc. |
| man-passwd-09 | 30-31 | Members and permissions modeled as Unix-style group membership | default | unverifiable | ∅ | Design statement; unverifiable black-box. |
| man-passwd-10 | 31-32 | `/etc/passwd` is read-only, one row per member with role, status, groups | output | `cat /etc/passwd` | ⚠️ | Read-only roster with one row per member and role/status/groups columns is true, but note the stray `alice  member  invited  —` row (bare "alice", not an email) left by earlier test agents: it is a 4th row in the roster. Read-only-ness of the file itself: writes to /etc are 🔒 per rules. |
| man-passwd-11 | 35 | Header USER ROLE STATUS GROUPS | output | `cat /etc/passwd` | ✅ | Header printed exactly: `USER             ROLE     STATUS    GROUPS`. |
| man-passwd-12 | 36 | Owner shown as `(all — implicit)` | output | `cat /etc/passwd` | ❌ | Actual: `alice@acme.test  admin  active  binmgr cronmgr configmgr nsmgr acme acme+tokens audit-metavars-ns audit-metavars-ns+tokens`. Role is `admin`, not `owner`; groups are listed explicitly, no `(all — implicit)`. (Dev stack has no owner user, so an owner row is unobserved: partly dev-stack artifact; but admins demonstrably appear with groups.) |
| man-passwd-13 | 37 | Member groups listed space separated | output | `cat /etc/passwd` after grants | ✅ | Groups are space-separated (double-space) in the roster: `binmgr  cronmgr  configmgr  nsmgr  acme  acme+tokens` (existing grants; no grant performed). |
| man-passwd-14 | 38 | Invited member shows `invited` and `—` | output | `adduser x@y.com; cat /etc/passwd` | ⚠️ | Existing invited row shows `member  invited  —` (stray `alice` row) so the format matches, but I did not create it with adduser (forbidden). Stray row shows a bare name, not an email. |
| man-passwd-15 | 41-42 | Mutating team via `adduser`/`deluser` is owner-only | command | Run as non-owner; expect denial | 🔒 | No non-admin user in acme/globex. Human: sign in as a plain member and run `adduser z@y.com`; expect denial. Note: the doc says owner-only but dev users have role `admin`; no `owner` role observed. |
| man-passwd-16 | 42 | Or from dashboard's Members tab | UI | Web UI Members tab | 🖥 | Open dashboard, Members tab, confirm invite/remove controls. |
| man-passwd-17 | 42-43 | Owners have implicit full access and never appear in a group | output | `groups <owner>` | ❌ | Not observable as documented: no owner in dev stack; admins alice/bob/carol DO list groups (`groups` -> `binmgr cronmgr configmgr nsmgr acme acme+tokens`). See 12. |
| man-passwd-18 | 47 | A group is one of three things (manager, namespace, namespace+tokens) | default | `groups` for varied members | ⚠️ | Groups observed beyond the three kinds: `nsmgr` (undocumented; appears for every admin), plus `acme`, `acme+tokens`, `audit-metavars-ns(+tokens)` which fit the namespace kinds. Also manager list in doc omits nsmgr. grep of docs/shell and docs/manual finds no mention of nsmgr. |
| man-passwd-19 | 49-50 | `binmgr` grants edit of `/bin` | command | see team-16 | 🔒 | Needs a write to /bin by a binmgr vs non-binmgr member; human: as member without binmgr try `echo x > /bin/t.sh` (expect denial), then adduser grant. Note sh -c is zero-write by design. |
| man-passwd-20 | 49-50 | `cronmgr` grants edit of `/cron.d` | command | see team-17 | 🔒 | Same as 19 for /cron.d and cronmgr. |
| man-passwd-21 | 49-50 | `configmgr` grants edit of `/etc` | command | see team-18 | 🔒 | Same as 19 for /etc and configmgr. |
| man-passwd-22 | 51 | `<namespace>` group gives read (query) of `/log/<namespace>` | command | see team-24 | 🔒 | No non-admin member; human: grant `adduser u@x.com <ns>`, sign in as u, `tail /log/<ns>/...` works, other ns denied. |
| man-passwd-23 | 52 | `<namespace>+tokens` mints producer tokens for namespace | command | see team-25 | 🔒 | Human: grant `<ns>+tokens` then as that member `weft token create <ns>...`. |
| man-passwd-24 | 53 | +tokens independent of read; one, other, or both | command | see team-26 | 🔒 | Human: grant only +tokens, confirm token mint works but log read is denied. |
| man-passwd-25 | 55-56 | `+tokens` uses `+` not `:` | default | see team-27 | ∅ | Design rationale; unverifiable black-box (see 26). |
| man-passwd-26 | 56 | `+` is illegal in a namespace name | command | `namespace create 'a+b'` rejected | 🔒 | Needs `namespace create` which is forbidden. Human: `weft namespace create "a+b"` expect rejection. |
| man-passwd-27 | 60 | `adduser <email>` invites; sends invite email | command | Run; check inbox | 🔒 | Mutation + third-party email. Human: `adduser dana@acme.com`, check inbox. |
| man-passwd-28 | 61 | Invitee joins when they accept (or next sign in) | default | needs-human (second email) | 🔒 | Needs a human accepting the invite email. |
| man-passwd-29 | 62 | `adduser <email> <group>` adds member to group | command | Run on active member; `groups` | 🔒 | Mutation. Human: `adduser bob@acme.com payments` then `groups bob@acme.com`. |
| man-passwd-30 | 62-64 | If email not on team yet, invited now and email goes out | command | `adduser new@x.com payments` | 🔒 | Mutation + email. Human: `adduser new@x.com payments`, check output says invited. |
| man-passwd-31 | 64-65 | Grant doesn't apply until accept; re-run afterwards | command | check `groups` before/after | 🔒 | Human: after step 30, `groups new@x.com` shows no payments; accept invite; rerun adduser; groups shows payments. Read-side: invited row (stray alice) shows `groups=—` via `id alice` -> `uid=alice role=member groups=—`, consistent with no groups. |
| man-passwd-32 | 65-66 | Invited-but-not-accepted gives a clear error rather than silent no-op | output | run again; capture error | 🔒 | Human: `adduser <invited-email> payments` and capture error text. Not run. Note the stray `alice` invited row exists and could be used. |
| man-passwd-33 | 67 | `deluser <email> <group>` revokes group | command | Run; `groups` | 🔒 | Mutation. Human: `deluser bob@acme.com payments`, `groups bob@acme.com`. |
| man-passwd-34 | 68 | `deluser <email>` removes member entirely | command | Run; `cat /etc/passwd` | 🔒 | Mutation. Human: `deluser x@y.com`; `cat /etc/passwd` row gone. |
| man-passwd-35 | 69 | `groups [<email>]` lists member's groups, yours if omitted | command | `groups` vs `groups x@y.com` | ✅ | `groups` (self) -> `alice@acme.test: ...`; `groups bob@acme.test` -> bob's groups. Output prefixed with `email:`. Caveat: `groups bob@acme.com` -> `no such member` (doc example domain not valid on dev stack, expected). |
| man-passwd-36 | 70 | `id [<email>]` shows member's role and groups | command | Run | ✅ | `id carol@acme.test` -> `uid=carol@acme.test role=admin groups=binmgr,cronmgr,configmgr,nsmgr,acme`. |
| man-passwd-37 | 72 | adduser/deluser owner-only | command | see 15 | 🔒 | See 15. |
| man-passwd-38 | 72-73 | Group grants only after accept; no staging | command | see 31 | 🔒 | See 31. |
| man-passwd-39 | 78 | `adduser dana@acme.com` # invite | command | Run | 🔒 | Mutation + email; forbidden. |
| man-passwd-40 | 79 | `adduser dana@acme.com payments` lets dana read payments logs | command | see 29 | 🔒 | See 29. |
| man-passwd-41 | 80 | `payments+tokens` lets dana mint producer tokens | command | see team-35 | 🔒 | Human: grant payments+tokens then mint token as dana. |
| man-passwd-42 | 81 | `cronmgr` lets dana edit /cron.d | command | see team-36 | 🔒 | Human: grant cronmgr then dana edits /cron.d. |
| man-passwd-43 | 82 | `deluser dana@acme.com payments` revokes read | command | see team-37 | 🔒 | See 33. |
| man-passwd-44 | 83 | `deluser dana@acme.com` removes from org | command | see 34 | 🔒 | See 34. |
| man-passwd-45 | 84 | `groups dana@acme.com` valid | command | Run | ✅ | Syntax valid: `groups bob@acme.test` works (dana@acme.com itself returns `no such member`, since not on team). |
| man-passwd-46 | 89 | SEE ALSO `help builtins` | xref | `help builtins` | ✅ | static: `help builtins` is a key in help_corpus_gen.go (line 102). Live view 🖥. |
| man-passwd-47 | 89 | SEE ALSO `help index` | xref | `help index` | ⚠️ | static: no `"help index"` key in help_corpus_gen.go; only bare `help` (and `help --help`) maps to HelpIndex. `help index` likely does not resolve; confirm by typing `help index` in `weft sh` (🖥). |
| man-passwd-48 | 7-10 | Pages `man adduser`, `man deluser`, `man groups` all map to this same page (no `man id`) | command | `man id` result | ⚠️ | static: no `man id` key in corpus (only passwd/adduser/deluser/groups). Live `man id` 🖥; expect not found. |
| man-passwd-49 | 11 | Section (5) for a command-ish page; mixed with (1) elsewhere | output | `man passwd` header | ✅ | static: banner is PASSWD(5) (see 02). Mixed (1)/(5) sections is a style matter. |

## Style observations
- Content heavily duplicates manual/team.md (risk of drift); man page says "three things" while team.md says "exactly two families" (manager + namespace groups), which is an inconsistency to resolve.
- `man id` not among topics although `id` is in SYNOPSIS.
- "dashboard's Members tab" ambiguity (see team.md).
- Banner PASSWD(5) vs other pages (1).
- No warp, Windows, "watch", or coming-soon mentions.

## Findings
- **man-passwd-12 / 17 (doc lines 36-43)**: doc shows owner row `alice@acme.com owner active (all — implicit)` and says owners never appear in a group. Actual: dev users have role `admin` and their groups are listed explicitly (`binmgr cronmgr configmgr nsmgr acme acme+tokens ...`). Probably dev-stack has no owner, but nothing observed matches the sample; also `owner-only` (lines 41, 72) vs role `admin`. Ambiguous/needs an owner account to confirm. Also emails in the sample (`@acme.com`) vs dev `@acme.test` (harmless).
- **man-passwd-18 (line 47-53)**: `nsmgr` group appears for every admin and is undocumented; "one of three things" is incomplete (doc side stale, or product group not documented). Proposed text: add `- **nsmgr** - create and manage namespaces.` (confirm meaning with owner of the feature).
- **man-passwd-47 (line 89)**: `help index` has no key in the help corpus (only bare `help`). Proposed: `help builtins, help` unless `help index` is wired elsewhere. Verify live.
- **man-passwd-48 (lines 7-10)**: `id` is in SYNOPSIS but there is no `man id` topic (static). Proposed: add `man id` to topics, or leave as-is.
- **man-passwd-10/14 stray row**: roster contains `alice  member  invited  —` (bare name, not email) from earlier test agents. It does not invalidate any claim but makes `cat /etc/passwd` show 4 rows; `id alice` -> `uid=alice role=member groups=—`.
- Format note (man-passwd-08): `id` prints `uid=<email> role=<r> groups=a,b` (comma separated) and `groups` prints `<email>: a b`; the doc does not show this output format; a reader may expect space-separated groups from `id`. Documentation gap.
- Unknown email gives `no such member: <email>`; undocumented but fine.
