# `git` on a shared core (replacing `wit`)

**Status:** ✅ Phases 0–4 done 2026-10-11 (Phase 5 optional, not started). Departures from the design:
deletes are soft (`deleted_at`), so there is no tombstone trigger — saving over a deleted name revives
the row in place and history continues; `AutoCommitter` gained `RemoveFile` (reverting an addition);
`origin_version_id` (migration 086) tells a truncated history from a true first version, since
`predecessor_id` is `ON DELETE SET NULL`; the vim popup still amends the script's own HEAD by name
rather than going through `Reword(id)` (its amend is now restricted to the version's author, in the store). Migration 087 keeps a deletion in the log after the name is re-saved (history rows carry `deleted_at`). warp: gitcore in v0.2.43–v0.2.46. vimgym: branch `gitcore`, PR graybeardcorp/vimgym#7, not merged.
**Touches:** warp (new package), vimgym (backend + import swap), weft (backend, small schema change, `wit` → `git`, docs).
**Source:** the `wit.md` audit at the repo root (2026-10-11).

## Goal

One git implementation, two storage models:

- **vimgym** keeps its in-memory, full-DAG repo (branches, index, work tree,
  rebase/bisect/stash) for the git course.
- **weft** keeps its Postgres model: each `/bin` script and `/cron.d` entry
  has a live row and its own linear history table. There is no git object
  database and no staging area.

Both drive the same command parser, revision parser and renderers. weft's
command is renamed from `wit` to **`git`** and becomes a **strict subset of
git**: every command it accepts behaves and prints the way git does, and
every real git command it doesn't implement is refused with an explicit
"unsupported" error. We have no users besides Paul, so argument order,
revision meaning and the command set change freely. There is no compatibility
shim for the old `wit` grammar, and no `wit` alias.

## Where things are today

- **vimgym `pkg/gitsim`**, about 6k lines.
  - `commands.go` is a 1,472-line dispatcher plus porcelain (the user-facing
    commands).
  - `log.go` and `diff.go` already produce output shaped like real git's.
  - `repo.go` holds the in-memory `Repo`. vimgym's graders and scenario
    builders read its fields directly: `r.WorkTree` 143 times, `r.Index`
    109 times, `r.Commits` 83 times, and so on across `gitscenario`,
    `toolchallenge` and `training`.
  - The course machinery (`challenge.go`, `gen.go`, `interrogation.go`,
    `tool.go`, `ValidateLevel`, `Stars`) is in the same package.
- **weft `coordinator/wit.go`**, 551 lines. It registers a bash command
  with path-first grammar: `log`, `show`, `diff`, `checkout`, `amend`.
  History comes from the `capture_*_history` triggers in `store/schema.sql`.
  - They fire on UPDATE only, copy `OLD` into `*_history` when
    `content_hash` changes, and trim to 5 history rows. That means 6
    versions per object, including HEAD.
  - The history tables have **no foreign key** to the live rows.
    `DeleteShellFunction` deletes only the live row. Its history rows
    survive as orphans that can no longer be reached by name, and the
    content at the moment of deletion is lost, because no trigger fires
    on DELETE.
- **Defects in today's `wit`**, confirmed by reading the code:
  - **`wit checkout` skips every check on the normal save path.**
    `runCheckout` writes with a raw `gorm` `Updates`. That skips:
    - the mounts' `CanManageFunctions`/`CanManageWatches` permission
      checks, so a member with read-only access can overwrite a script;
    - `ClassifyBinScript`;
    - the cron tier and interval checks;
    - `LowerForStorage`, so a cron checkout leaves a stale `lowered_plan`.
  - **Every older version's date is off by one.** `LogShellFunction` sets
    a history row's `AuthoredAt` to `r.CapturedAt`, which is when the
    version was *replaced*, not when it was written.
  - **`HEAD^N` has the wrong meaning.** In `wit` it means "N versions
    back". In git it means "the Nth parent".

## Decisions

### 1. Package layout: the core lives in warp, the backends live with their apps

| Where | What |
|---|---|
| `warp/pkg/gitcore` | The interfaces, command dispatch, flag parsing (gitsim's strict `unknownFlag` discipline), revision parsing, the `log`/`show`/`diff`/`status`/`blame`/`rev-parse` renderers, the LCS diff, and the auto-commit porcelain: `restore`, `checkout <rev> -- <path>`, `revert`, `commit --amend`. It imports nothing from vimgym or weft. |
| `vimgym/pkg/gitsim` | The in-memory `Repo`, with its exported fields unchanged, satisfying `gitcore.Repository`. It also keeps the porcelain only it can do (`add`, `commit`, `branch`, `switch`, `checkout <branch>`, `merge`, `rebase`, `cherry-pick`, `reset`, `reflog`, `stash`, `bisect`) as a `CommandProvider`, plus the course machinery. The package keeps its name, so vimgym's import churn is small. |
| `weft/witrepo` (new) | The Postgres-backed backend. It builds commits from the history tables and routes writes through `mounts`. |

warp's package vocabulary stays generic: repo, commit, tree, path.
Nothing in warp mentions bin, cron, org or pseudofile.

What this layout costs:

- **gitcore must export what gitsim's porcelain reuses.** That means
  `ResolveRev`, `UnifiedDiff(path, old, new)`, `RejectUnknownFlags` and the
  commit/log line renderers. These become warp's public API, so they need
  doc comments and stable signatures.
- **gitcore's own tests need a backend.** A test-only
  `gitcore/internal/testrepo` (a small in-memory `Repository` +
  `AutoCommitter`) covers that. The real cross-check is vimgym's suite
  producing byte-identical output (Phase 1 gate).

### 2. Backend interface: shared reads, writes enabled per backend

Every backend implements one read interface:

```go
type Repository interface {
    Head() (ref string, id ObjectID, err error) // ref "" = detached
    Refs() (map[string]ObjectID, error)         // refs/heads/*, refs/tags/*
    Commits() ([]*Commit, error)                // all reachable; core does prefix resolution and range walks
    Commit(id ObjectID) (*Commit, error)
    Tree(id ObjectID) (map[string]string, error) // path → content
}

type Commit struct {
    ID       ObjectID
    Parents  []ObjectID
    Boundary []string // paths whose earlier history was truncated; see Decision 3
    Author   string
    Email    string
    Time     time.Time
    Message  string
}
```

Writes come through two optional interfaces. The core checks for each one
with a type assertion:

```go
// AutoCommitter: the work tree *is* HEAD; every write is a commit (weft).
type AutoCommitter interface {
    WriteFile(path, content, message string) (ObjectID, error)
    Reword(id ObjectID, message string) error
}

// CommandProvider: backend-owned porcelain the core doesn't generalize (vimgym).
type CommandProvider interface {
    Commands() map[string]CommandFunc
}
```

**How a subcommand is dispatched:**

1. The backend's `CommandProvider` table is checked first.
2. Then the core's own commands. If a core command needs an interface the
   backend doesn't implement, it falls through to step 3.
3. Anything else that is a **real git subcommand** gets the unsupported
   error. gitcore keeps a list of real git subcommand names (`rebase`,
   `stash`, `push`, `submodule`, and so on), and the embedder supplies the
   product name:

   ```
   git: 'rebase' is a git command, but weft doesn't support it
   ```

   vimgym can pass its own name or leave its current wording.
4. Anything that isn't a git command at all gets git's own error:

   ```
   git: 'rebsae' is not a git command. See 'git --help'.
   ```

   That way a typo reads as the user's mistake, while a missing feature
   reads as ours.

Flags stay on git's normal path (`error: unknown option`). Making every
real-but-unimplemented flag say "unsupported" would mean listing all of
git's flags. If that's wanted later, it can start with the handful users
actually try.

**gitsim's merge/rebase/bisect/stash code is moved unchanged, not
rewritten on top of the shared core.** Rewriting it that way would be a
1,500-line refactor with exactly one user. weft can never merge, because
each of its objects has a single linear history. It can be generalized if
a second full-history backend ever appears.

**There is no hook API in the core.** In weft, the backend's `WriteFile`
does the job of hooks: it calls the existing `shellcore.MountFS` write.
So every `git` mutation runs the same checks as `vi`, `cp` and `>`:
permissions, `ClassifyBinScript`, `ParseCron`, the tier and interval
limits, and `LowerForStorage`. vimgym doesn't need hooks.

### 3. Building a git history from weft's tables

- **Repo root is `/`.** Tracked paths are `bin/<name>` and
  `cron.d/<name>`. Paths can be written the ways git accepts: `/bin/foo`
  (absolute, inside the work tree), `bin/foo`, or a cwd-relative `foo`
  after `cd /bin`.
- **One commit per version row**, plus one for each live row's HEAD
  version. Every commit touches exactly one path.
- **One linear branch, `main`.**
  - Commits are ordered by `authored_at`, with `(kind, id)` as the
    tiebreak.
  - Each commit's single parent is the previous commit across the whole
    org. `HEAD` is the org's latest change.
  - `git log` is the org's combined change history; `git log --
    /bin/foo` is one file's history.
- **The tree at commit C** contains the latest version of each object
  written at or before C, minus deleted objects.
  - The backend builds trees in one pass.
  - An org has tens of objects and hundreds of versions, so it loads
    everything on each invocation: no `LIMIT` pushdown and no caching.
    This follows the guidance that pushdown shortcuts aren't worth it
    when sizes are bounded.
- **Commit ID** is `sha1(kind ‖ object_id ‖ version_id)`.
  - It prints as 40 hex characters, abbreviated to 7 where git
    abbreviates.
  - It must not change when a HEAD version moves into the history table,
    so a stable `version_id` is needed (Phase 3).
- **Author and date.** The author is resolved from `authored_by_user_id`
  as `Name <email>`, the same lookup `ownerLabel` does today. The date is
  `authored_at`.
- **Empty messages get a generated one:** `Update bin/foo`,
  `Create bin/foo` or `Delete bin/foo`. git has no empty messages, and
  `cp`/`>`/`tee` writes never prompt for one.
- **The 5-version retention cap stays.** Truncated history is shown the
  way a shallow clone shows it:
  - The oldest retained version of a path that once had earlier versions
    is a **boundary** for that path.
  - `git log --decorate` marks it `grafted`. Its patch shows the file as
    added, which is what real git prints for a shallow boundary commit.
  - `git blame` prefixes lines from it with `^`, which is git's own
    boundary marker.
  - Revisions older than the boundary are unresolvable: `fatal: bad
    revision`.

  The difference from git is that the truncation is per path rather than
  one depth cutoff for the whole repo. That goes on the
  shortcomings whitelist.

### 4. weft's `git` surface

| Command | Behavior |
|---|---|
| `git log [--oneline] [--decorate] [-p] [--stat] [-n N] [--author=] [--grep=] [-S] [--since=] [--until=] [<rev>\|<a>..<b>] [[--] <path>…]` | Git's default `log` format. |
| `git show [<rev>]`, `git show <rev>:<path>` | Commit plus patch; `<rev>:<path>` prints the file's content at that revision. Replaces `wit show <path> <ref>`. |
| `git diff <a> <b> [--] <path>…`, `git diff <a>..<b> …` | Plain `git diff` prints nothing, because the work tree is always clean. Replaces `wit diff <path> a..b`. |
| `git status` | `On branch main` / `nothing to commit, working tree clean`. |
| `git restore --source=<rev> <path>…` | Writes `<rev>`'s content as a new commit through `WriteFile`. Also brings back a deleted file (Phase 3 tombstones). Replaces `wit checkout <path> <ref>`. |
| `git checkout <rev> -- <path>…` | Same as `restore`, in git's older spelling. `git checkout <rev>` without `--` (which would move HEAD) is refused as unsupported. |
| `git revert <rev>` | Reverse-applies a single-path commit as a new commit. If the path changed since `<rev>`, git's conflict text is printed and nothing is written. |
| `git commit --amend -m <msg>` | Rewords the latest commit. Refused if someone else wrote it, so you can't reword a teammate's save that landed after yours. Replaces `wit amend`. |
| `git commit` (without `--amend`) | `nothing to commit, working tree clean`, exit 1, the same as real git on a clean tree. |
| `git blame <path>` | Cheap to compute on a per-path linear history, and it answers "who changed this line of the cron". |
| `git rev-parse <rev>` | Useful in `/bin` scripts. |
| `add`, `rm`, `mv`, `branch`, `switch`, `merge`, `rebase`, `cherry-pick`, `reset`, `stash`, `bisect`, `tag`, `reflog`, `push`, `pull`, `fetch`, `clone`, … | `git: '<cmd>' is a git command, but weft doesn't support it` |

**Revision syntax**, implemented in the core so vimgym gains it too:

- `HEAD`, `@`
- `HEAD~N`, `HEAD^`, `HEAD^^`
- `HEAD^N`, meaning the Nth *parent* (git's meaning)
- abbreviated IDs of at least 4 characters, and branch names
- `<a>..<b>` and `<rev>:<path>`

**Revisions are no longer scoped to a file.** `HEAD` is now the org's
latest change, not the file's. The usual workflow becomes `git log --
/bin/foo`, then `git show <hash>`, or
`git show <hash>:bin/foo`.

**Color and paging.** The core emits plain text. The embedder adds color
and paging only when output goes straight to the terminal, the same rule
vimgym's `highlightDiffOutput`/`pageOutput` already use.

- weft runs bash on the server, so it needs to know whether a command's
  stdout is the terminal. The interpreter tracks that in
  `bash.Interpreter.outputIsTerminal`, which is unexported today; warp
  needs to expose it to `ExternalCmdFunc`s.
- `--color=always|never` overrides the default.
- This removes `wit`'s always-on color codes.

**The vim popup keeps working.** The save returns the new `ObjectID`, and
the popup's `AmendVersionMessage` becomes `Reword(thatID)`. It never
targets "HEAD", because a teammate's save could land in between.

**Renaming the command from `wit` to `git`** also means:

- adding `git` to the reserved-names list in `shell/shell.go`, so a
  `/bin/git` function can't shadow it;
- moving the help and man page from `wit` to `git`.

## Phases

### Phase 0: close the `wit checkout` permission hole (weft, ~0.5d)

This is a security bug, so it doesn't wait for the redesign. Following the
house bug rule:

1. Write a failing test: a member without `binmgr` runs
   `wit checkout /bin/x HEAD^` and the source changes. Write the same for
   `cronmgr`.
2. Fix it by routing the checkout write through the request's
   `mounts.Config` / MountFS `WriteFile` instead of raw `gorm`. That one
   change also closes the skipped `ClassifyBinScript`, tier/interval and
   `lowered_plan` checks. Write a test for each.

Phase 4 deletes this code, but the hole is live in prod until then.

### Phase 1: create gitcore and move vimgym onto it, no behavior change (warp + vimgym, ~2d)

1. Create `warp/pkg/gitcore`:
   - the interfaces and dispatcher;
   - the read-side renderers lifted from gitsim's `log.go`, `diff.go`
     and `cmdShow`/`cmdStatus`;
   - the revision parser (`resolveRef`/`parseTilde`) and `unknownFlag`;
   - their tests, running against `internal/testrepo`.

   Tag warp.
2. In vimgym, make `gitsim.Repo` satisfy `gitcore.Repository`. Its
   mutating commands become its `CommandProvider`, and
   `ExecuteArgs` delegates to the gitcore dispatcher.
3. `SHORTCOMINGS_WHITE_LIST.md` splits in two:
   - the entries about rendering and the command surface move to warp
     with gitcore, as warp does for awk/sed/jq;
   - the entries about the object model (non-content-addressed hashes,
     UTC-only dates) stay in vimgym.
4. **Gate:** every vimgym git test passes with byte-identical output,
   including `TestLevelGateAdmitsEverySolution` and the interrogation
   suites.

### Phase 2: core features both apps want (warp, ~2d)

- The full revision syntax listed above.
- `log --since/--until` (relative and absolute dates), and
  `--decorate` with the `grafted` boundary marker.
- `show <rev>:<path>`, `rev-parse`, `blame` (with the `^` boundary
  prefix).
- The real-git subcommand list and the unsupported-versus-unknown error
  split, with the product name passed in by the embedder.
- `--color=auto|always|never`, and the exported is-terminal hook on the
  bash interpreter.
- The auto-commit porcelain: `restore`, `checkout <rev> -- <path>`,
  `revert` and `commit --amend` against `AutoCommitter`.

### Phase 3: weft schema (weft, ~1d)

A migration at a version above `store/schema.sql`'s max, followed by
`make collapse` after the prod deploy. The 5-version cap is unchanged.

1. **`version_id`** (bigint from a sequence) on `shell_functions` and
   `cron_watches`. It is assigned whenever `content_hash` changes and
   copied into `*_history` by the triggers. Backfill existing rows. This
   is what keeps commit IDs stable.
2. **`authored_at`** on the live rows (set alongside `version_id`) and on
   the history tables (copied from `OLD`).
   - The backfill sets each history row to its predecessor row's
     `captured_at`, which is when that version took over.
   - The oldest retained row has no predecessor, so it falls back to its
     own `captured_at`.
   - This also fixes the off-by-one dates.
3. **Tombstones on delete.** Add a `BEFORE DELETE` trigger that captures
   `OLD` into history with `deleted = true`.
   - History rows already survive a delete, because there is no foreign
     key to the live row. Only the final content and the deletion event
     are lost today.
   - With the trigger, `git log` shows deletions and `git restore` can
     bring a deleted script back.
   - Re-creating a deleted name gets a new object ID, so its history
     starts fresh, the same as git after an rm and re-add. The previously
     orphaned rows become reachable through the global log.

### Phase 4: weft backend and the `git` command (weft, ~2–3d)

1. **Build `witrepo`** as specified in Decision 3. The history
   construction gets table-driven tests:
   - interleaved bin and cron edits;
   - a rename, where `name` changes but the object ID doesn't, and git
     shows a delete plus an add;
   - a delete tombstone, and a resurrect;
   - an object past the cap, which must produce the boundary marker.
2. **Route `WriteFile` through the request's MountFS**, the same object
   the shell uses. Check that the coordinator has one at
   `coordinator.go:1354`; if it only has `Store`, pass the mounts
   `Config` through.
3. **Replace `coordinator/wit.go`** with a thin `RegisterCommand("git",
   …)` that builds gitcore over `witrepo`.
   - Delete `ResolveShellFunctionRef`, `ResolveCronWatchRef`, the ref
     handling in `Amend*Message`, `refLabel` and `formatVersionDiff`.
   - `go-difflib` can then be removed if nothing else uses it.
4. **Point the vim popup's message write at `Reword(id)`.**
5. **Rename the docs and generated help from `wit` to `git`:**
   - `docs/manual/wit.md` becomes `git.md`;
   - the `wit` sections of `scripts.md`, `crons.md` and `sharing.md`;
   - `docs/shell/help-builtins.md`;
   - regenerate `shell/help_corpus_gen.go` and
     `frontend/src/generated/docs/*`.

   User-facing docs call the command "a subset of git" and don't name
   warp.
6. **Update the remaining `wit` callers:**
   - `cmd/weft-demo-seed`;
   - the onboarding lessons in `coordinator/onboarding/lessons.go`. That
     file has uncommitted work in the tree right now, so coordinate
     before touching it.

### Phase 5 (optional): track `/etc`

`/etc/dashboards/*` and `/etc/notify.yaml` have no history today. Add
`dashboard_history` and `etc_history` with the same trigger design, and
include them when `witrepo` builds the history. Then `git log -- /etc`
and `git restore` work for config too. Defer until someone wants to roll
back a dashboard.
