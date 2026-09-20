---
id: manual-wit
title: wit — version history for /bin and /cron.d
web:
  section: wit
  category: customize
  order: 65
---

## Version history with `wit`

Every save to `/bin/<name>` or `/cron.d/<name>` is captured in a
short, browseable history. If you fat-finger an edit, push a
script that breaks an alert at 2am, or just want to see what your
cron looked like last week, the `wit` builtin has you covered.

It looks like a tiny git: `wit log`, `wit show`, `wit diff`,
`wit checkout`, `wit amend`. The mental model is the same — each
save is a "commit" on a single linear branch — but the surface is
deliberately small. There's no merging, no rebasing, no remotes.
Just a snapshot trail of recent versions.

## What's tracked

| Path                  | Tracked? |
|-----------------------|----------|
| `/bin/<name>`         | ✓ — saves to scripts produce versions |
| `/cron.d/<name>`      | ✓ — saves to watches produce versions |
| `/etc/notify.yaml`    | ✗ |
| `/log/...`            | ✗ |
| `/tmp/...`            | ✗ |

`wit` commands against any non-tracked path return
`history not tracked here`.

## How a version is identified

Two ways to refer to a version, both accepted by every `wit`
subcommand that takes a ref:

- `HEAD` — the live, current version.
- `HEAD^` — one back. `HEAD^2` is two back. Up to `HEAD^5`
  (we keep the five most recent past versions).
- A **content hash** — `wit log` shows a 7-character short hash
  next to each entry. Any unique prefix of 4 or more hex chars
  resolves to the matching version. Same content always produces
  the same hash; if you check out an old version that happens to
  match the current one, the new "save" event reuses the hash.

Sequential `v1, v2, v3` numbering is **not** a stable identifier.
A `wit checkout` of an older version shifts what "v3" means.
Hashes don't drift.

## The five-version cap

We keep the **last five** past versions per entity. Every save
beyond that drops the oldest one. Versions older than five saves
ago are gone — there's no recycle bin.

Keep this in the back of your head when iterating quickly. If
you've been hammering `:w` ten times a minute polishing a cron,
the version you started from this morning is no longer reachable.

## Commands

### `wit log <path>`

Newest-first listing of versions, including HEAD.

```
weft> wit log /bin/scan
* a4fb0c2  HEAD     2026-04-25 09:42  alice    "fix the awk filter"
  3d2e91f  HEAD^    2026-04-25 09:15  alice    "tighten error pattern"
  c01b745  HEAD^2   2026-04-24 11:03  bob
  1e8a02c  HEAD^3   2026-04-22 17:33  bob      "initial pass"
```

Columns: HEAD-marker, short hash, ref, when (last-edit time of
that version), author (the user who originally wrote it), commit
message subject (in quotes when set, blank otherwise).

Add `-p` to interleave a unified diff between consecutive
versions:

```
weft> wit log -p /bin/scan
* a4fb0c2  HEAD     ...  alice    "fix the awk filter"
diff a4fb0c2 ← 3d2e91f:
- cat /log/myco/api/main.log | grep ERROR | wc -l
+ cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' | head -100 | wc -l

  3d2e91f  HEAD^    ...  alice    "tighten error pattern"
diff 3d2e91f ← c01b745:
- cat /log/myco/api/main.log | grep -i error | wc -l
+ cat /log/myco/api/main.log | grep ERROR | wc -l

  c01b745  HEAD^2   ...  bob
  ...
```

### `wit show <path> <ref>`

Print a version's content. Useful for re-running an old script
without committing to a checkout:

```
weft> wit show /bin/scan HEAD^2
cat /log/myco/api/main.log | grep ERROR | wc -l

weft> wit show /cron.d/errors HEAD^
# schedule: every 5m
cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' | wc -l | alert above 0 --notify=ops
```

Crons show their `# schedule:` directive on the first line so
you can read the version as a complete cron file.

### `wit diff <path> <a>..<b>`

Unified diff between two refs. Headers carry the short hash and
author of each side:

```
weft> wit diff /bin/scan HEAD^2..HEAD
--- a/scan (c01b745, bob)
+++ b/scan (a4fb0c2, alice)
- cat /log/myco/api/main.log | grep ERROR | wc -l
+ cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' | head -100 | wc -l
```

The range syntax is one rigid form: `<a>..<b>`. Both endpoints
must resolve to a real version; either side can use any ref form.

### `wit checkout <path> <ref>`

Make an older version the new HEAD. The current HEAD drops into
history (where it can be checked out again later), and the
restored version becomes the live row.

```
weft> wit checkout /bin/scan HEAD^2
checked out /bin/scan @ c01b745  (now HEAD)

weft> wit log /bin/scan
* c01b745  HEAD     2026-04-25 10:08  bob      ← restored from c01b745
  a4fb0c2  HEAD^    2026-04-25 09:42  alice    "fix the awk filter"
  3d2e91f  HEAD^2   2026-04-25 09:15  alice    "tighten error pattern"
  c01b745  HEAD^3   2026-04-24 11:03  bob      ← same content, different timestamp
  1e8a02c  HEAD^4   2026-04-22 17:33  bob      "initial pass"
```

A few subtleties worth knowing:

- The original author of a checked-out version stays as that
  version's author. `wit log` after a checkout shows bob (the
  original writer) as HEAD's author, not whoever did the checkout.
- The user who *did* the checkout is recorded in the displaced
  version's "displaced by" field. So if you accidentally check
  out the wrong thing and want to know who, `wit log` will say.
- The 5-cap can drop versions during a checkout if the chain was
  already at five entries.

### `wit amend [-m MSG] <path> [<ref>]`

Set or replace a commit message. Default ref is `HEAD`:

```
weft> wit amend -m 'restore working version' /bin/scan
amended a4fb0c2: "restore working version"

weft> wit amend -m 'first cut' /bin/scan HEAD^3
amended 1e8a02c (HEAD^3): "first cut"
```

`wit amend` on HEAD doesn't create a new history row — adding a
label to the live version isn't authoring new content. Same for
amending a history ref: only the message column changes, the
underlying version is untouched.

## Commit messages at save time

Right after a successful save (`:w` in vi, or any other write to
`/bin/<name>` or `/cron.d/<name>`), the shell drops you into a
follow-up vi buffer pre-filled with a comment-prefixed prompt:

```
# Commit message for /bin/scan.
# One or more lines; the first line is the subject shown in `wit log`.
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.

█
```

Type a message, `:wq` records it. `:q!` skips it. Either way you
end up back at the shell prompt.

A few notes:

- Comment lines (`#`-prefixed) and blank lines are stripped before
  recording. An entirely-comment buffer = no message.
- The first line is the subject; it shows up next to the version
  in `wit log` output (truncated if needed).
- Multi-line messages are fine — `wit show --message <ref>`-style
  commands aren't a thing yet, but the full body is preserved
  for future use.

If you skipped the popup and want to add a message after the fact,
that's what `wit amend` is for.

## Permissions

`wit log` / `wit show` / `wit diff` are read-only — same access
as `cat /bin/foo` or `cat /cron.d/foo`.

`wit checkout` and `wit amend` are writes. Same namespace
permissions as the existing FS write path; if you can save the
file, you can check out an older version of it or amend a
message.

## What `wit` does NOT do

- **No branches, no merges.** It's a linear snapshot trail.
- **No history for `/etc/notify.yaml` or `/tmp`.** Notify config
  is rare-edit shared state; `/tmp` is intentionally ephemeral
  with its own 48-hour expiry.
- **No recovery from `rm`.** Deleting a script or cron deletes
  its history with it. There's no `wit reflog`. If you have
  history you'd want to keep, `wit checkout` to a new version
  before `rm`-ing.
- **No diff against arbitrary text.** `wit diff` only compares
  two refs of the same entity. To diff against external content,
  pipe it through your shell's `diff` or paste into `/tmp` and
  `diff /tmp/<file> /tmp/<other>`.
