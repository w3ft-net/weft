---
id: manual-git
title: git — version history for /bin, /cron.d and /etc
web:
  section: git
  category: customize
  order: 65
---

## Version history with `git`

Every save to a script in `/bin`, a cron in `/cron.d`, a dashboard
in `/etc/dashboards` or `/etc/notify.yaml` is captured in a short,
browseable history. If you fat-finger an edit, push a script that
breaks an alert at 2am, or just want to see what your cron or
dashboard looked like last week, the `git` command has you covered.

It is a **subset of git**. Everything it accepts behaves and prints
the way git does, so what you already know works: `git log`,
`git show`, `git diff`, `git blame`, `git restore`. Everything it
doesn't implement is refused by name:

`git --help` lists what is supported.

```
weft> git rebase -i HEAD~3
git: 'rebase' is a git command, but weft doesn't support it
```

while a typo reads as a typo:

```
weft> git rebsae
git: 'rebsae' is not a git command. See 'git --help'.
```

There is one branch, `main`. Every save is a commit on it, in the
order the saves happened, across your whole org. There is no
staging area: a write *is* the commit, which is why there is no
`git add`, no merging, no rebasing, no remotes.

## What's tracked

| Path                  | Tracked? |
|-----------------------|----------|
| `/bin/<name>`         | ✓ — saves to scripts produce commits |
| `/cron.d/<name>`      | ✓ — saves to crons produce commits |
| `/etc/dashboards/<name>.yaml` | ✓ — saves to dashboards produce commits |
| `/etc/notify.yaml`    | ✓ — saves to the alert aliases produce commits |
| `/etc/passwd`, `/etc/usage` | ✗ — generated, not stored |
| `/log/...`            | ✗ |
| `/tmp/...`            | ✗ |

The repository root is `/`, and tracked paths are `bin/<name>`,
`cron.d/<name>`, `etc/dashboards/<name>.yaml` and `etc/notify.yaml`.
Write a path any way git accepts it: `/bin/scan`,
`bin/scan`, or just `scan` after `cd /bin`. A path outside `/bin`
and `/cron.d` is refused: `fatal: '/tmp/x' is outside the
repository (only /bin, /cron.d, /etc/dashboards and /etc/notify.yaml are tracked)`.

## Revisions

`HEAD` is the org's **latest change**, not one file's — use a path
to narrow to a file (`git log -- /bin/scan`). Revisions work as in
git:

- `HEAD`, `@`, and the short hash `git log` prints (4 or more
  characters).
- `HEAD~N` — N saves back. `HEAD^` is `HEAD~1`. (`HEAD^2` is the
  *second parent*, as in git; this history has none, so it is an
  error.)
- `<a>..<b>` — the commits in `<b>` that aren't in `<a>`.
- `<rev>:<path>` — a file's content as of a revision.

## The five-version cap

We keep each file's **latest six** versions (the live one plus the
five before it). Older versions are gone — there's no recycle bin.

The oldest version still kept for a file is shown the way a
shallow clone shows its edge: `git log` marks it `grafted`, its
patch shows the file as newly added, `git blame` prefixes its
lines with `^`, and revisions older than that don't resolve
(`fatal: bad revision`). Unlike a real shallow clone, the cutoff is
per file rather than one depth for the whole repository.

## Commands

### `git log`

```
weft> git log --oneline
a4fb0c2 (HEAD -> main) fix the awk filter
3d2e91f tighten error pattern
c01b745 Update cron.d/errors
1e8a02c initial pass
```

`git log` is the org's combined history; `git log -- /bin/scan` is
one file's. It takes `--oneline`, `-p`, `--stat`, `-n N`/`-N`,
`--author=`, `--grep=`, `-S<text>` (commits that added or removed
the text), `--since=`/`--until=` (`"2 days ago"`, `2026-10-01`),
`--decorate`/`--no-decorate`, `--all`, and a `<rev>` or
`<a>..<b>`. Anything else is refused:
`fatal: unrecognized argument: --bogus`.

A save with no message gets a generated one — `Create bin/scan`,
`Update bin/scan`, `Delete bin/scan` — because git has no empty
messages.

### `git show`

```
weft> git show HEAD~2:bin/scan
cat /log/myco/api/main.log | grep ERROR | wc -l
```

`git show <rev>` prints a commit and its patch;
`git show <rev>:<path>` prints the file as it was then. Crons show
their `# schedule:` line first, so a version reads as a complete
cron file.

### `git diff`

```
weft> git diff HEAD~2 HEAD -- /bin/scan
```

`git diff <a> <b>` and `git diff <a>..<b>` compare two revisions,
optionally limited to paths after `--`. Plain `git diff` prints
nothing: there is no working copy to differ from — the live file
*is* HEAD.

### `git status`

Always `On branch main` / `nothing to commit, working tree clean`,
for the same reason.

### `git blame <path>`

Who last changed each line of a script or cron:

```
weft> git blame /bin/scan
^1e8a02c (Bob   2026-04-22 17:33:12 +0000 1) cat /log/myco/api/main.log
a4fb0c2f (Alice 2026-04-25 09:42:01 +0000 2) | awk 'LEVEL == "ERROR"'
```

A `^` marks a line that predates what the five-version cap kept.

### `git restore`, `git checkout <rev> -- <path>`

Make an older version current again:

```
weft> git restore --source=HEAD~3 /bin/scan
weft> git checkout HEAD~3 -- /bin/scan        # same thing, older spelling
```

This writes the old content as a **new commit** (`Restore bin/scan
from 0174c77`) — history is never rewritten — through the same path as `vi`: your permissions, the
Lua/bash classification, and (for crons) your plan's schedule
limits all apply. It also brings back a script that was deleted,
if its history is still there. `git checkout <rev>` without `--`
would move HEAD, which has no meaning here, and is refused.

### `git revert <rev>`

Undo one commit with a new commit. If the file changed again since
that commit, git's conflict is reported and nothing is written:

```
error: could not revert 3d2e91f... tighten error pattern
hint: bin/scan has changed since that commit, so the revert would conflict; nothing was written
```

### `git commit --amend -m <message>`

Rewords the latest commit. You can reword only a file's current
version, only one you wrote yourself, and only with permission to
edit that file — so you can't reword a teammate's save that landed
after yours. A plain `git commit` finds nothing to commit, as in
git on a clean tree.

### `git rev-parse`

`git rev-parse HEAD`, `--short`, `--abbrev-ref HEAD`, `--verify`
— handy inside `/bin` scripts.

## Colour and pipes

Output that goes straight to your terminal is coloured. Output
headed into a pipe or a redirect is plain text, so
`git log -p | grep foo` and `git show HEAD:bin/scan > /tmp/scan`
do what you expect. `--color=always` or `--color=never` overrides.

## Commit messages at save time

Right after a successful save in vi of a file git tracks (a script,
cron, dashboard or `notify.yaml`), the shell drops you into a
follow-up vi buffer pre-filled with a comment-prefixed prompt:

```
# Commit message for /bin/scan.
# One or more lines; the first line is the subject shown in `git log`.
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.

█
```

Type a message, `:wq` records it. `:q!` skips it. Either way you
end up back at the shell prompt. Comment lines (`#`-prefixed) and
blank lines are stripped; an entirely-comment buffer records no
message. If you skipped the popup and want to add one later,
that's `git commit --amend -m`.

## Deleting and bringing back

Deleting a script or cron shows up as a `Delete bin/<name>`
commit, and `git restore --source=<rev> <path>` brings it back
with its history intact. Saving a new file over a deleted name does
the same thing: the deletion stays in the log, followed by the new
save.

## Permissions

`git log`, `show`, `diff`, `status`, `blame` and `rev-parse` are
read-only — the same access as `cat /bin/foo` or `cat /cron.d/foo`.

`git restore`, `checkout -- <path>`, `revert` and `commit --amend`
are writes. They need the same group as editing the file does:
`binmgr` for `/bin`, `cronmgr` for `/cron.d`, `configmgr` for `/etc`.
Restoring an old dashboard still validates it: if it refers to a cron
that has since been deleted, the restore is refused.

## What `git` here does NOT do

- **No working tree, no staging.** `add`, `rm`, `mv`, `stash`,
  `reset`, `clean` don't exist; every write is already a commit.
- **No branches, merges or rebases.** One linear history.
- **No remotes.** `clone`, `fetch`, `pull`, `push` are refused.
- **No history for `/tmp`, `/etc/passwd` or `/etc/usage`.** `/tmp` is
  intentionally ephemeral; the other two are generated (from group
  membership and metering), so there is nothing stored to roll back.
  Change the team with `adduser` / `deluser`.
- **Slack setup is not in `/etc/notify.yaml`.** Only the alias file
  is versioned; connecting Slack doesn't create a commit.
- **Patterns are RE2, not POSIX.** `--author` and `--grep` use Go
  regular expressions, so `+`, `?` and `|` are operators.
