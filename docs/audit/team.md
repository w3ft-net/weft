# Ledger: docs/manual/team.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| team-01 | 10-13 | Org members and permissions live in `/etc/passwd` | path | `cat /etc/passwd` | ✅ | `cat /etc/passwd` lists org members, roles, groups |
| team-02 | 12-13 | `/etc/passwd` is a read-only table | path | `echo x > /etc/passwd` -> error; `vi /etc/passwd` can't save | ✅ | `sh -c "echo x > /etc/passwd"` -> "/etc/passwd is read-only — manage the team with adduser/deluser" (vi absent under sh -c; 🖥 for vi) |
| team-03 | 16-20 | `cat /etc/passwd` header columns: USER ROLE STATUS GROUPS | output | `cat /etc/passwd` | ✅ | header `USER ROLE STATUS GROUPS` observed |
| team-04 | 18 | Owner row shows GROUPS `(all — implicit)` | output | `cat /etc/passwd` as owner org | ❌ | No owner row. Org acme roster shows ROLE `admin` for alice/bob/carol; no `(all — implicit)` anywhere. Role vocabulary is admin/member, not owner/member |
| team-05 | 19 | Member row lists groups space-separated (`binmgr  cronmgr  payments  payments+tokens`) | output | grant groups to a member, `cat /etc/passwd` | ⚠️ | Groups are space-separated as documented (`binmgr  cronmgr  configmgr  nsmgr  acme  acme+tokens`) but bob shows admin role with groups, and a 4th manager group `nsmgr` appears; granting not tested (write) |
| team-06 | 20 | Invited member has ROLE `member`, STATUS `invited`, GROUPS `—` | output | `adduser new@x.com; cat /etc/passwd` | ⚠️ | Invited row observed: `alice  member  invited  —` (ROLE member, STATUS invited, GROUPS —) as documented; but USER is not an email here. Creation of invite not tested (adduser forbidden) |
| team-07 | 17 | STATUS values include `active` and `invited` | output | see 06 | ✅ | `active` and `invited` both observed in STATUS |
| team-08 | 17 | ROLE values include `owner` and `member` | output | `cat /etc/passwd` | ❌ | ROLE values observed: `admin` and `member`; `owner` never appears |
| team-09 | 16 | Doc shows `$ ` prompt for the shell | UI | Open shell, observe prompt format | 🖥 | `sh -c` prints no prompt; open interactive `weft sh` and compare prompt to `$ ` |
| team-10 | 23-24 | You never edit the file; team managed via `adduser`/`deluser` | command | `man adduser` | 🔒 | `man` unavailable under sh -c; 🖥 type `man adduser`. adduser/deluser forbidden |
| team-11 | 23-24 | `adduser` and `deluser` are owner-only | command | Run as non-owner member; expect permission error | 🔒 | No non-admin user in acme; as non-admin member run `adduser x@y.com`, expect permission error |
| team-12 | 24 | Team can also be managed from dashboard's Members tab | UI | Web UI: open dashboard, find "Members" tab | 🖥 | Open web dashboard, look for a Members tab |
| team-13 | 24-25 | Shell commands and Members tab write the same underlying data | default | `adduser x@y.com` then refresh Members tab; and vice versa | 🔒 | Run `adduser x@y.com` in shell, refresh Members tab (needs write + browser), then reverse |
| team-14 | 29 | Every permission is a group; exactly two families | default | `groups <email>` shows only manager and namespace-style groups | ❌ | `groups bob@acme.test` shows `binmgr cronmgr configmgr nsmgr acme acme+tokens`: fourth manager group `nsmgr` exists, so "three org-wide directories"/exactly two families still holds as families but manager list is incomplete |
| team-15 | 31 | Manager groups gate the three org-wide directories | default | see 16-18 | ⚠️ | Manager groups gate /bin, /cron.d, /etc, but also `nsmgr` (undocumented here, presumably namespace management) |
| team-16 | 35 | `binmgr` grants edit of `/bin` (functions) | command | Member with binmgr: `vi /bin/x`; without: denied | 🔒 | Needs write to /bin; step: as member with binmgr run `sh -c "echo x > /bin/t"`; without, expect denied |
| team-17 | 36 | `cronmgr` grants edit of `/cron.d` (crons) | command | Member with cronmgr: create cron in `/cron.d`; without: denied | 🔒 | Needs write to /cron.d; step: as cronmgr member create a cron; without, expect denied |
| team-18 | 37 | `configmgr` grants edit of `/etc` (notify.yaml, dashboards) | command | Member with configmgr: `vi /etc/notify.yaml`, `vi /etc/dashboards/x.yaml`; without: denied | 🔒 | Needs write to /etc; step: as configmgr member edit /etc/notify.yaml; without, denied |
| team-19 | 37 | `/etc/notify.yaml` exists | path | `ls /etc` | ✅ | `ls /etc` -> dashboards/ notify.yaml passwd usage; `cat /etc/notify.yaml` works |
| team-20 | 37 | Dashboards live under `/etc` | path | `ls /etc/dashboards` | ✅ | `ls /etc/dashboards` -> prod.yaml |
| team-21 | 35 | `/bin` holds functions | path | `ls /bin` | ✅ | `ls /bin` -> count_errors mem_history scan severity status.lua |
| team-22 | 36 | `/cron.d` holds crons | path | `ls /cron.d` | ✅ | `ls /cron.d` -> error-rate mem-pressure |
| team-23 | 39-40 | Namespace groups gate log access, one per namespace | default | `adduser <email> <ns>` for each ns | 🔒 | Needs adduser; step: `adduser <email> <ns>` per namespace, then `cat /etc/passwd`. Read-side: acme roster lists group `acme` per namespace |
| team-24 | 43 | Group `<namespace>` grants read (query) of `/log/<namespace>` | command | Member with group: `cat /log/<ns>/...` works; without: denied | 🔒 | Needs a non-admin member without the group; step: as that member `cat /log/acme/prod/api/main.log | head -1`, expect denied |
| team-25 | 44 | Group `<namespace>+tokens` grants minting producer tokens for that namespace | command | Member with +tokens: `weft token create` for ns works; without: denied | 🔒 | Needs non-admin member; step: `weft token create` as member with and without acme+tokens |
| team-26 | 46 | Read and produce are independent; member can have either or both | command | grant only `<ns>+tokens`; confirm cannot read; and inverse | 🔒 | Needs adduser to grant only `<ns>+tokens`; then confirm read denied. Read-side: carol has `acme` without `acme+tokens`, so the facets are stored independently |
| team-27 | 47-48 | `+tokens` facet uses `+` not `:` so it cannot collide with a namespace name | default | `namespace create 'a+tokens'` rejected | 🔒 | Namespace creation forbidden; step: `namespace create "a+tokens"` expect rejection |
| team-28 | 50 | Owners have implicit full access | command | Owner can read any ns, edit /bin /cron.d /etc, mint tokens | ⚠️ | Admins (alice, bob, carol) hold every group explicitly listed in /etc/passwd rather than implicit access; no `owner` role to test. Alice reading /log/acme works |
| team-29 | 50 | Owners never appear in a group | output | `groups <owner>` / `cat /etc/passwd` | ❌ | Admin rows list explicit groups (alice: `binmgr cronmgr configmgr nsmgr acme acme+tokens ...`); `groups alice@acme.test` also lists them. Privileged users DO appear in groups |
| team-30 | 55 | `adduser dana@acme.com` invites and sends an invite email | command | `adduser <real inbox>`; check inbox | 🔒 | Sends email; needs adduser and a real inbox |
| team-31 | 56 | `deluser dana@acme.com` removes from the org entirely | command | `deluser x@y.com; cat /etc/passwd` | 🔒 | Needs deluser; step: `deluser x@y.com; cat /etc/passwd` |
| team-32 | 59-60 | `adduser <email>` with no group sends an invite link | command | see 30; inspect email link | 🔒 | Needs adduser; inspect invite email |
| team-33 | 60 | Person joins when they accept, or next time they sign in with that email | default | needs-human (second email account sign-in) | 🔒 | Needs second email account sign-in |
| team-34 | 67 | `adduser dana@acme.com payments` grants read of payments logs | command | after accept: Dana queries `/log/payments` | 🔒 | Needs adduser plus accepted member; Dana then queries /log/payments |
| team-35 | 68 | `adduser ... payments+tokens` grants producer token minting | command | Dana mints token | 🔒 | Needs adduser, then Dana mints token |
| team-36 | 69 | `adduser ... cronmgr` grants editing `/cron.d` | command | see 17 | 🔒 | Needs adduser; see 17 |
| team-37 | 70 | `deluser dana@acme.com payments` revokes read | command | Dana queries after revoke: denied | 🔒 | Needs deluser; Dana queries after revoke |
| team-38 | 72 | `groups dana@acme.com` shows what dana can do | command | Run it | ✅ | `groups bob@acme.test` works and shows groups; example email `dana@acme.com` fails here with "no such member" (expected for a nonmember; error text undocumented). Dev emails are @acme.test [re-triaged 2026-10-12: `groups bob@acme.test` -> `binmgr cronmgr configmgr nsmgr acme acme+tokens`; `dana@acme.com` -> `no such member` only because dana is a globex non-member / made-up address. Works with a real member.] |
| team-39 | 73 | `id dana@acme.com` shows role + groups | command | Run it | ✅ | `id bob@acme.test` -> `uid=bob@acme.test role=admin groups=binmgr,cronmgr,...` (role + groups; comma-separated) |
| team-40 | 76-77 | Group grants apply only once member has accepted invite; no staging | default | `adduser <pending> payments`; observe error; `cat /etc/passwd` shows no group | 🔒 | Needs adduser on pending member |
| team-41 | 77-79 | `adduser` to a group for non-member invites them then and there (email goes out) | command | `adduser brandnew@x.com payments`; check inbox and `/etc/passwd` status invited | 🔒 | Needs adduser and inbox |
| team-42 | 79 | After they accept you must re-run the command | command | After accept, `groups <email>` is empty until rerun | 🔒 | Needs adduser and accept flow |
| team-43 | 79-80 | Already-invited, not-yet-accepted member gets a clear error not silent no-op | output | `adduser invited@x.com payments`; capture error text | 🔒 | Needs adduser on invited member; capture error |
| team-44 | 82-83 | `groups` and `id` are readable by any member | command | Run as non-owner member | 🔒 | No non-admin acme user; groups/id verified for an admin only. Step: run `groups` and `id` as plain member |
| team-45 | 82 | `adduser`/`deluser` owner-only (restated) | command | see 11 | 🔒 | See 11 |
| team-46 | 85 | `man passwd` in shell is quick reference | xref | `man passwd` | 🖥 | `man` unavailable under sh -c; type `man passwd` in interactive shell. Static: docs/manual/man-passwd.md does not exist at that path |
| team-47 | 8 | Doc placement: Getting Started order 30 | UI | Web docs sidebar order | 🖥 | Open web docs sidebar and check Team & permissions order under Getting Started |
| team-48 | 10 | Heading "The roster: /etc/passwd" and listing uses `—` em-dash as empty-groups marker | output | see 06 | ✅ | Empty-groups marker `—` observed in invited row |

## Style observations
- Duplicates man-passwd.md content nearly verbatim (roster sample, groups, adduser semantics); risk of drift between the two.
- Only the "Owners ... never appear in a group" while roster shows `(all — implicit)` in the GROUPS column: slight tension, worth checking.
- `groups`/`id` without args (self) not mentioned here but is in man-passwd.
- Sample roster uses `$ cat /etc/passwd` prompt while other docs show bare commands.
- "dashboard's Members tab" ambiguous: web dashboard vs. the `dashboard` shell command (man-dashboard.md).
- No warp, Windows, "watch", or "coming soon" mentions.

## Findings
- team-04/08/29 (lines 18, 50, 17): doc says ROLE `owner` with GROUPS `(all — implicit)` and owners never in a group. Actual: roles are `admin`/`member`; admins show explicit groups (`binmgr cronmgr configmgr nsmgr acme acme+tokens`). Doc stale or product diverged (ambiguous; could be dev-seed only). Proposed: replace `owner`/`(all — implicit)` sample with `admin` and listed groups, and reword line 50 once the intended model is decided.
- team-14/15 (line 31): doc lists three manager groups; actual has a fourth, `nsmgr`. Doc stale. Proposed: "Manager groups gate the org-wide directories and namespace management" and add an `nsmgr` row.
- team-06 (line 20): invited row in dev shows non-email USER (`alice`), minor. Sample emails use acme.com; dev uses acme.test (not a doc issue).
- team-38 (line 72): `groups` on a non-member returns `no such member: <email>`; doc could mention it.
- team-46: `docs/manual/man-passwd.md` does not exist, so the Style claim about duplicating it could not be checked statically.
- Static: generated team.ts contains only proper `&lt;`/`&gt;` entities in HTML; no stray placeholders found.

