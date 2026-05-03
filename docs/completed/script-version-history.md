# Lightweight version history for /bin and /cron.d

**Status:** plan — discussion before implementation.

## Why

Today, every save to `/bin/<name>` or `/cron.d/<name>` overwrites the
previous content. Users who fat-finger an edit, or who want to roll
back to the version that was working an hour ago, are out of luck.
With user attribution now in place (`created_by_user_id`), this is
the natural follow-up: keep the previous versions around, attribute
them, let users browse and restore.

The shape: a Postgres-trigger-driven append-only history per entity,
exposed via a small set of shell commands. Each entity (one bin
script or one watch) keeps a linked list of past versions, ordered
by save time. No git-style branching, no merging — just "what did
this look like before."

## Decisions

Locked in after review:

- **D-1: per-entity history tables.** Two new tables —
  `shell_function_history` and `cron_watch_history` — each mirroring
  the parent table's schema. Cleaner than a unified
  `content_history(entity_type, entity_id, …)`: each parent's
  schema-evolution story stays independent, and joins read trivially.
  ("Entity" here = one logical thing being versioned: one /bin
  script, or one watch.)
- **D-2: capture on UPDATE only.** `rm /bin/foo` deletes the entity
  and its history together. DELETE-capture creates discoverability
  dead ends — once a script is gone, the user has no entry point to
  ask "what was this called?" If we later want a safety net for
  destructive `rm`, it's better expressed as a `--force`
  prompt-when-history-exists or a `/trash` view, not as silent
  history retention.
- **D-3: linked list via `predecessor_id`.** Each history row points
  at the previous version's history row. The live row in the parent
  table is the implicit head. Restoring an old version writes that
  content forward as the new live row; the trigger captures the
  just-displaced live row into history with `predecessor_id`
  pointing at the previously-most-recent history row.
- **D-4: content-addressed history with git-style refs and
  commands.** See "Refs and commands" below — this is the biggest
  change from the first draft.
- **D-5: cap at 5 versions per entity.** When a sixth save would
  push history to 6 entries, the trigger drops the oldest history
  row. Versions past the cap "fall off the face of the earth" —
  unrecoverable. (5 is enough headroom for "I just messed up and
  want to undo a few saves" without unbounded growth.)
- **D-6: no backfill of initial history rows.** Pre-existing
  entities don't get synthetic history rows from the migration.
  `wit log` shows just HEAD until the user updates — backfill
  rows would be duplicate data and "never updated" should look
  different from "updated once."
- **D-7: namespace under `wit` subcommands.** Rather than dumping
  `log` / `show` / `diff` / `checkout` into the global builtin
  pool — where they'd collide with future use — every command goes
  under a single `wit` builtin that dispatches: `wit log`,
  `wit show`, `wit diff`, `wit checkout`, `wit amend`. Keeps the
  toplevel namespace clean and signals the cluster of "this is the
  version-control surface."
- **D-8: commit messages with in-editor capture at save time.**
  Each version carries a free-text `message` column (parent table
  for HEAD, history table for past versions). After a successful
  `:w` on `/bin/<name>` or `/cron.d/<name>`, the shell automatically
  opens a follow-up vi buffer pre-filled with a comment-prefixed
  prompt; saving the buffer records the message, `:q!` skips. This
  is part of v1 — typing `wit amend <path> -m '...'` after every
  save was the alternative, and the friction kills the feature.
- **D-9: file-less subcommands error out.** `wit amend`,
  `wit checkout`, `wit diff`, `wit show` all require an explicit
  `<path>` argument. No "default to the last-saved entity in
  this shell session" cleverness — a long-lived shell makes that
  ambiguous and the error from a missing path is cheap to fix.

## Schema

Add `content_hash` and `message` columns to the parent tables so
the live row carries the same canonical identifier and commit
message as past versions do in history:

```sql
ALTER TABLE shell_functions
    ADD COLUMN content_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN message      TEXT NOT NULL DEFAULT '';
ALTER TABLE cron_watches
    ADD COLUMN content_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN message      TEXT NOT NULL DEFAULT '';
```

`content_hash` is `SHA-256(source)` for shell functions, and
`SHA-256(schedule || '\n' || pipeline)` for cron watches —
deterministic, content-addressed, displayed as 7-char short hex
in user-facing surfaces (matching git's default abbreviation).

`message` is free-text, set by the user via `wit amend` (default
empty on first save). May be multi-line. The first line is treated
as the subject in `wit log` output.

Two new tables:

```sql
CREATE TABLE shell_function_history (
    id              BIGSERIAL PRIMARY KEY,
    function_id     BIGINT NOT NULL,
    -- No FK to shell_functions(id). The trigger fires on UPDATE
    -- only, so a row in history corresponds to a row that exists or
    -- existed in shell_functions at capture time. After a DELETE
    -- there's no history left to dangle (rm wipes both); after an
    -- UPDATE the live row is still there.
    customer_id     BIGINT NOT NULL,
    name            TEXT   NOT NULL,
    source          TEXT   NOT NULL,
    language        TEXT   NOT NULL,
    content_hash    TEXT   NOT NULL,
    message         TEXT   NOT NULL DEFAULT '',
    captured_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- User who displaced this version (the one whose UPDATE put it
    -- into history). NULL when the path that wrote didn't supply
    -- userID (cron executor, migration backfill).
    captured_by_user_id TEXT,
    -- Original author of this captured version, copied from
    -- shell_functions.created_by_user_id at capture time.
    authored_by_user_id TEXT,
    predecessor_id  BIGINT REFERENCES shell_function_history(id),
    UNIQUE (function_id, captured_at)
);

CREATE INDEX idx_sfh_function_id   ON shell_function_history (function_id, captured_at DESC);
CREATE INDEX idx_sfh_customer_name ON shell_function_history (customer_id, name, captured_at DESC);
CREATE INDEX idx_sfh_content_hash  ON shell_function_history (function_id, content_hash);
```

Parallel `cron_watch_history` with `pipeline` / `schedule` /
`alert_*` columns.

### Content hashing

Computed at write time in the store layer, before INSERT/UPDATE:

```go
hash := sha256.Sum256([]byte(source))
contentHash := hex.EncodeToString(hash[:])
```

For watches, the content is `schedule + "\n" + pipeline` so a
schedule-only change produces a new hash. The `alert_*` projection
columns aren't part of the hash — they're derivative of pipeline
content.

Hashes use full SHA-256 in storage (64 hex chars); user-facing
displays render the first 7 chars (`a4fb0c2`). Lookups by hash
prefix match any unique abbreviation — error out if ambiguous,
matching git's behaviour.

## Trigger

Two responsibilities: (1) capture OLD into history before the
UPDATE applies, (2) enforce the 5-version cap by dropping the
oldest history row when adding a new one would push us past five.

```sql
CREATE OR REPLACE FUNCTION capture_shell_function_history()
RETURNS TRIGGER AS $$
DECLARE
    last_history_id BIGINT;
    captured_by TEXT;
    history_count INT;
BEGIN
    -- captured_by is the user driving this UPDATE. Arrives via a
    -- per-transaction Postgres GUC the store sets at the top of
    -- every Save call:
    --
    --   SET LOCAL weft.acting_user_id = '<userID>';
    --
    -- SET LOCAL is automatically scoped to the transaction; no
    -- leakage between requests. Falls back to NULL when the caller
    -- didn't set it (cron executor, migration backfill, direct SQL
    -- from a maintenance shell).
    BEGIN
        captured_by := current_setting('weft.acting_user_id');
    EXCEPTION WHEN undefined_object THEN
        captured_by := NULL;
    END;

    -- Predecessor pointer: most recent existing history row.
    SELECT id INTO last_history_id
    FROM shell_function_history
    WHERE function_id = OLD.id
    ORDER BY captured_at DESC
    LIMIT 1;

    INSERT INTO shell_function_history (
        function_id, customer_id, name, source, language, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.customer_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.created_by_user_id, last_history_id
    );

    -- Enforce 5-version cap: count history rows for this entity,
    -- drop the oldest if we're past 5. We keep exactly 5 history
    -- rows + 1 live row = 6 versions total at steady state.
    SELECT count(*) INTO history_count
    FROM shell_function_history
    WHERE function_id = OLD.id;

    IF history_count > 5 THEN
        DELETE FROM shell_function_history
        WHERE id = (
            SELECT id FROM shell_function_history
            WHERE function_id = OLD.id
            ORDER BY captured_at ASC
            LIMIT 1
        );
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER capture_shell_function_history
    BEFORE UPDATE OF source, language ON shell_functions
    FOR EACH ROW
    EXECUTE FUNCTION capture_shell_function_history();
```

Note the `UPDATE OF source, language` clause: the trigger fires only
when content-defining columns are in the UPDATE's SET list.
Message-only updates from `wit amend HEAD` don't fire it — adding
a commit message to the live row should not produce a new history
row (no content displaced; the user's just labeling what's there).

Parallel function + trigger for `cron_watches`, scoped to
`UPDATE OF schedule, pipeline`. The `RETURN NEW` at the end of the
BEFORE-UPDATE trigger lets the UPDATE proceed unmodified.

### How the trigger learns the acting user

`weft.acting_user_id` is a per-transaction Postgres GUC the store
sets at the top of every Save call:

```go
func (s *Store) SaveShellFunction(customerID uint, userID, name, source string) error {
    return s.db.Transaction(func(tx *gorm.DB) error {
        if userID != "" {
            if err := tx.Exec("SET LOCAL weft.acting_user_id = ?", userID).Error; err != nil {
                return err
            }
        }
        // existing insert/update logic, plus content_hash computation
    })
}
```

Empty userID skips the SET — the trigger sees a missing GUC and
stores NULL in `captured_by_user_id`. `SET LOCAL` is scoped to the
transaction; no leakage between requests.

## Refs and commands

Identifying a specific version: any of these forms works wherever a
ref is accepted.

| Ref form        | Meaning                                                      |
|-----------------|--------------------------------------------------------------|
| `HEAD`          | The current live row.                                        |
| `HEAD^`         | One version back (most recent history row).                  |
| `HEAD^N`        | N versions back. Errors for N > 5 (cap).                     |
| `<hash-prefix>` | Match a content hash by any unique prefix (≥ 4 chars).       |
| `<a>..<b>`      | Range used by `diff`; ordered, both endpoints valid refs.    |

Sequential `vN` numbering is **not** a ref — it's a render
convention only, shown next to the hash in `log` output. We can't
rely on `v3` being stable: if the user restores an old version,
"v3" before vs after the restore mean different things. The hash
doesn't drift.

All commands are registered as subcommands under a single `wit`
builtin (whimsy: weft + git). One toplevel name; everything
version-control lives under it:

| Command                              | What it does                                                  |
|--------------------------------------|---------------------------------------------------------------|
| `wit log <path>`                     | Newest-first list: hash, when, who, message subject           |
| `wit log -p <path>`                  | Same, with unified diff between consecutive versions inline   |
| `wit show <path> <ref>`              | Print the content at the given ref                            |
| `wit diff <path> <a>..<b>`           | Unified diff between two refs                                 |
| `wit checkout <path> <ref>`          | Make the given ref the new live version                       |
| `wit amend [-m MSG] [<path> [<ref>]]`| Set/replace a version's commit message; default ref is HEAD   |

`<path>` is a `/bin/foo` or `/cron.d/<watch>` reference — the
`wit` dispatcher routes to the right history table. Other
namespaces error out (`wit log /tmp/foo` → "history not tracked
here").

### Examples

```
weft> wit log /bin/scan
* a4fb0c2  HEAD     2026-04-25 09:42  alice    "fix the awk filter"
  3d2e91f  HEAD^    2026-04-25 09:15  alice    "tighten error pattern"
  c01b745  HEAD^2   2026-04-24 11:03  bob
  1e8a02c  HEAD^3   2026-04-22 17:33  bob      "initial pass"

weft> wit log -p /bin/scan
* a4fb0c2  HEAD     2026-04-25 09:42  alice    "fix the awk filter"
diff a4fb0c2 (HEAD) ← 3d2e91f (HEAD^):
- cat /log/api.log | grep ERROR | wc -l
+ cat /log/api.log | awk '$LEVEL == "ERROR"' | head -100 | wc -l

  3d2e91f  HEAD^    2026-04-25 09:15  alice    "tighten error pattern"
diff 3d2e91f (HEAD^) ← c01b745 (HEAD^2):
- cat /log/api.log | grep -i error | wc -l
+ cat /log/api.log | grep ERROR | wc -l

  c01b745  HEAD^2   2026-04-24 11:03  bob
  ...

weft> wit show /bin/scan HEAD^2
cat /log/api.log | grep -i error | wc -l

weft> wit show /bin/scan c01
cat /log/api.log | grep -i error | wc -l

weft> wit diff /bin/scan HEAD^2..HEAD
--- a/scan (c01b745, bob)
+++ b/scan (a4fb0c2, alice)
- cat /log/api.log | grep -i error | wc -l
+ cat /log/api.log | awk '$LEVEL == "ERROR"' | head -100 | wc -l

weft> wit checkout /bin/scan HEAD^2
checked out /bin/scan @ c01b745  (now HEAD)

weft> wit amend -m 'restore working version'
amended a4fb0c2: "restore working version"
```

A few subtleties worth noting from the examples:

- `checkout HEAD^2` writes `c01b745` content forward as a new
  HEAD; the old HEAD (`a4fb0c2`) drops into history; everything
  shifts down by one. The 5-cap kicks in if the chain is full.
- `wit amend HEAD` sets the message on the live row only — does
  NOT trigger a new history capture (the trigger is scoped to
  `UPDATE OF source, language` in the schema above).
- `wit amend HEAD^2 -m 'something'` updates the message on a
  history row directly; no new version produced.
- `wit show /bin/scan c01` resolves the prefix; if `c01b745` is the
  only hash starting with `c01`, it matches. Ambiguity errors out
  with the candidates listed.

### Post-save message UX

Right after a successful `:w` (or `:wq` / `ZZ`) on `/bin/<name>`
or `/cron.d/<name>`, the shell drops the user into a follow-up vi
buffer pre-filled with a comment-prefixed prompt:

```
# Commit message for /bin/scan @ a4fb0c2.
# One or more lines; the first line is the subject shown in `wit log`.
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.

█
```

Cursor lands on the empty line below the comments. User types a
message → `:wq` records it. User changed their mind → `:q!`
records nothing. Either way, vi exits and the user is back at the
shell prompt. No second command needed.

**Where the logic lives.** This is shellcore work, not warp work.
shellcore already drives mode transitions; it adds:

- Track on every vi session: "did the user save to a versioning-
  eligible path during this session?" (i.e., a /bin or /cron.d
  path). Shellfs's `WriteFile` can return a flag, or shellcore
  can pattern-match on the path.
- When ModeVi exits *and* a versioning-eligible save happened,
  immediately re-enter ModeVi with a synthetic in-memory buffer
  whose initial content is the comment-prefixed prompt above.
  Track the saved path on the new mode instance.
- When the message-mode ModeVi exits via `:wq`, strip the comment
  lines from the buffer, trim, and call
  `store.AmendShellFunctionMessage(custID, name, "HEAD", message)`.
  Empty post-strip → record nothing.
- On `:q!`, do nothing.

No changes to warp's vi. The message buffer is a regular vi
session whose "path" is a special sentinel like `:wit-message:`
that shellfs's `WriteFile` recognizes and routes to a no-op so
`:w` mid-message-edit doesn't surprise the user. (Or: shellcore
intercepts the save attempt entirely and treats `:w` and `:wq`
identically inside message mode — saving means "record &
exit.") The exact details land in PR 2.

**Non-interactive contexts** (CLI's `weft sh -c '...'`, scripts
piping into the shell, the gRPC SaveFunction RPC) bypass vi
entirely; they use `wit amend` after the save if they want a
message, same as today's git users do `git commit -m`. The
in-editor popup is purely a quality-of-life feature for
interactive editing.

**Failure modes.**

- Save fails (validation error from store): vi shows the error,
  user retries; no message popup until a save succeeds.
- User types `:wq` with an empty message buffer: same as `:q!`;
  no message recorded.
- Network failure between save and message-record: the version
  is already on disk (saved before the popup); `wit amend` after
  the fact still works.

### Permissions

`wit log` / `wit show` / `wit diff` are read-only — same access
as `cat /bin/foo`. `wit checkout` and `wit amend` are writes —
same namespace permissions as `Save…` and the existing FS write
path.

### Aliases

We could expose `history` as an alias for `wit log`, `restore`
for `wit checkout`, `annotate` for `wit amend`. **Position: don't.**
Pick git's vocabulary as canonical, namespace it under `wit`, and
stick with it; aliases double the surface area without buying
clarity.

## Store API

Five new methods per entity (`shell_function` shown; `cron_watch`
is parallel). Existing `Save…` methods grow a `SET LOCAL
weft.acting_user_id` step inside their transaction, plus a
content-hash computation.

```go
// LogShellFunction returns the version chain newest-first,
// including the live row. At most 6 entries (5 history + HEAD).
func (s *Store) LogShellFunction(customerID uint, name string) ([]ShellFunctionVersion, error)

// ResolveShellFunctionRef parses a ref string ("HEAD", "HEAD^N",
// or hash prefix) and returns the matching version. Errors:
// ErrAmbiguousHash, ErrNotFound, ErrInvalidRef.
func (s *Store) ResolveShellFunctionRef(customerID uint, name, ref string) (*ShellFunctionVersion, error)

// ShellFunctionContent returns the raw source for a ref — what
// `wit show <path> <ref>` prints. Convenience over Resolve.
func (s *Store) ShellFunctionContent(customerID uint, name, ref string) (string, error)

// CheckoutShellFunction makes the given ref the new live version.
// Internally writes via SaveShellFunction so the trigger fires
// (capturing the just-displaced live row into history). userID is
// the user performing the checkout; recorded as captured_by on the
// new history row. authored_by stays with the original author.
func (s *Store) CheckoutShellFunction(customerID uint, userID, name, ref string) error

// AmendShellFunctionMessage sets/replaces the commit message on
// the version targeted by ref. For HEAD, this UPDATEs
// shell_functions.message — the trigger is scoped to UPDATE OF
// source, language so message-only edits don't capture into
// history. For history refs (HEAD^N or hashes), it UPDATEs the
// matching shell_function_history row.
func (s *Store) AmendShellFunctionMessage(customerID uint, name, ref, message string) error
```

`ShellFunctionVersion` collapses live + history rows into one
shape callers can iterate uniformly:

```go
type ShellFunctionVersion struct {
    ContentHash       string    // 64-char hex; render the first 7 in UI
    Source            string
    Language          string
    Message           string    // free-text commit message; "" if unset
    AuthoredAt        time.Time // captured_at for history; updated_at for live
    AuthoredByUserID  *string
    DisplacedByUserID *string   // nil for the live row (HEAD)
    IsHead            bool      // true for the live row, false for history rows
}
```

## ls -al integration

`ls -al /bin` already shows the current owner (per phase-1 user
attribution). With history in play, we could add columns showing
version count and the live row's short hash:

```
-rw-r--r--  alice   a4fb0c2   4 vers   214 B   2026-04-25 09:42  scan
-rw-r--r--  bob     8c1d3ab   1 ver     87 B   2026-04-22 17:03  cleanup
```

Nice to have, not required for v1. Defer until users ask.

## Migrations

One migration: `036_script_version_history.sql` (next free number
after the rebase).

Contents:

1. Add `content_hash TEXT NOT NULL DEFAULT ''` and
   `message TEXT NOT NULL DEFAULT ''` to `shell_functions` and
   `cron_watches`.
2. Backfill `content_hash` on existing rows by computing
   `SHA-256(source)` (or `SHA-256(schedule || '\n' || pipeline)`).
   `message` defaults to '' on existing rows — they have no
   commit messages by definition.
3. Create `shell_function_history` and `cron_watch_history` tables
   with the schema above.
4. Define the `capture_*_history()` trigger functions and attach
   `BEFORE UPDATE OF source, language` (and `OF schedule, pipeline`
   for cron) triggers on the parent tables. The column-scoping
   prevents `wit amend HEAD` from creating a spurious history row.
5. **No history-row backfill** for pre-existing entities. `wit log`
   shows just HEAD until someone updates — see "Backfill: history
   row vs implicit HEAD only" below for the rationale.

## Tests

TDD as everywhere else. The trigger has enough logic that I want
direct SQL-level tests for the capture invariants, plus
store-level + shell-level tests on top.

### SQL / trigger layer

- **Capture on UPDATE OF source.** Insert with content X, UPDATE
  source to Y, assert exactly one history row exists with content
  X, the old authored_by_user_id, and the old content_hash.
- **Message-only UPDATE does NOT capture.** Insert, then UPDATE
  message='hi'; assert zero history rows. (This is the column-
  scoped trigger contract.)
- **Predecessor chain.** Three content UPDATEs produce three
  history rows; row 3 → row 2 → row 1 → NULL via `predecessor_id`.
- **Five-version cap drops oldest.** Six content UPDATEs leaves
  exactly 5 history rows; the first save is gone.
- **acting_user_id propagation.** `SET LOCAL weft.acting_user_id`
  before UPDATE; the resulting history row's `captured_by_user_id`
  matches. Without SET, NULL.
- **No capture on INSERT.** First-time `Save…` doesn't add a
  history row.

### Store layer

- **Content-hash determinism.** `SaveShellFunction` with the same
  source twice produces matching hashes.
- **`LogShellFunction` order + count + message.** N content
  updates produce a log with min(N+1, 6) entries, newest first,
  including HEAD; messages flow through correctly.
- **`ResolveShellFunctionRef` forms.** HEAD, HEAD^, HEAD^3, hash,
  hash-prefix (≥4 chars), ambiguous prefix (errors), unknown hash.
- **`CheckoutShellFunction` round-trip.** Set source A, B, C, then
  checkout HEAD^2; current source = A; log shows the new HEAD with
  `displaced_by` pointing at the checking-out user, `authored_by`
  preserved from A's original author.
- **`AmendShellFunctionMessage` on HEAD.** Set message via amend;
  assert no new history row was created (column-scoped trigger).
  Re-fetch HEAD → message reflects the new value.
- **`AmendShellFunctionMessage` on a history ref.** Push a few
  versions, amend HEAD^2's message; the change persists on that
  history row only, others unchanged.

### Shell layer

- `wit log /bin/foo` lists versions in the right shape; messages
  appear in the subject column.
- `wit log -p /bin/foo` interleaves diffs between consecutive
  versions in the right place.
- `wit show /bin/foo HEAD^2` prints the right content.
- `wit diff /bin/foo HEAD^2..HEAD` produces a unified diff with
  short-hash file labels.
- `wit checkout /bin/foo <hash-prefix>` updates HEAD; subsequent
  `wit log` reflects the new chain.
- File-less subcommands error: `wit amend`, `wit show`,
  `wit diff`, `wit checkout` all error with usage when the path
  argument is missing.
- Cross-namespace errors: `wit log /tmp/foo` and `wit log
  /etc/usage` both report "history not tracked here" cleanly.

### Post-save message popup tests (shellcore)

- **Save on /bin triggers the message popup.** Open vi on
  /bin/foo, edit, `:wq`. Assert shellcore re-enters ModeVi with
  a buffer containing the comment-prefixed prompt and the right
  short hash.
- **`:wq` on the message buffer records the message.** Type
  "hello world", `:wq`. Assert HEAD's message is "hello world"
  (after stripping comments).
- **`:q!` on the message buffer skips.** Type a message, `:q!`.
  Assert HEAD's message is empty.
- **Empty message buffer = no record.** `:wq` on an unedited
  prompt-only buffer. Assert HEAD's message is empty.
- **Save on /tmp does NOT trigger the popup.** Edit /tmp/foo,
  `:wq`. Assert no follow-up mode transition.
- **Save fails: no popup.** Force a validation error (e.g.,
  invalid bash syntax in /bin/foo); vi shows the error; user
  exits without further action; no message popup.

## Rollout

Three PRs, each green standalone:

1. **Schema + triggers + store methods + acting-user GUC.** No
   user-visible changes. Tests cover capture / no-capture-on-
   message-only / cap / predecessor / hash determinism /
   `Resolve` / `Checkout` / `Amend` at SQL and store layers. The
   currently-blocked `SaveFunction` / `SaveWatch` gRPC userID
   extraction lands here too — both need the `SET LOCAL
   weft.acting_user_id` plumbing in the store wrappers.
2. **`wit` builtin + post-save message popup.**
   - `wit log` (incl. `-p`), `show`, `diff`, `checkout`, `amend`
     subcommands. Single dispatcher; subcommand errors out
     uniformly when `<path>` is missing.
   - Shellcore mode-transition logic that re-enters ModeVi with
     a comment-prefixed message buffer after a `/bin` or
     `/cron.d` save, records the message via
     `store.AmendShellFunctionMessage` (or its watch parallel)
     on `:wq`, skips on `:q!`.
3. **Manual updates** in `docs/manual/scripts.md` and a new
   `docs/manual/wit.md` covering the workflow end-to-end —
   ref grammar, the `wit` subcommands, and the post-save
   message flow.

## What this plan does NOT do

- Branching / merging / pulls. This is a snapshot trail, not a DAG.
  No `merge`, no `rebase`, no remotes.
- Diffs against arbitrary commits in customer git repos. The
  backing store is the only source of truth.
- Per-line blame. We capture entire-file versions; if you want to
  know which line was added when, `diff` two versions.
- History for `/etc/notify.yaml` or `/tmp`. Notify is rare-edit
  config (could add later); /tmp is intentionally ephemeral.
- Recovery from `rm`. Deleting an entity drops history with it.
  Future safety net via a `--force`-on-rm prompt or a `/trash`
  view, not blanket retention.
