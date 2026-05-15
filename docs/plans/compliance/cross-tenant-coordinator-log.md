# Plan: cross-tenant disclosure via the shared coordinator log

**This is a found defect, not a feature.** Surfaced in prod while
verifying the cron-path migration after the query-audit split shipped
(commit `33db933`, "coordinator+weftd: split query-audit stream onto its
own pseudofile" — the plan doc was deleted post-ship; git history is the
record). That split was scoped to the *self-reference* problem; it
incidentally narrowed but did **not** close this distinct
confidentiality issue in the same log stream.

## Mechanism

The coordinator is one shared multi-tenant process. It writes every
tenant's activity to its own stderr → `/log/weft/coordinator/*`:

- `query: app=<otherns>/… sql=…` — other tenants' SQL, including
  predicate values.
- `rewriter callback: plan error: … (pipeline="…")` — other tenants'
  cron pipeline text verbatim.
- `ingest-stream: … customer=<other> app=<otherns>/…` — other tenants'
  customer identifiers and app/namespace names.

Any principal who can read `/log/weft/coordinator/*` (the weft-ops
namespace — the one this MCP session authenticates into) therefore sees
other tenants' query/cron **content and identifiers**.

## Confirmed vs. not (do not overstate)

- **Leaking:** cross-tenant *metadata / query content* into an
  ops-readable log. A real disclosure surface, independent of row
  security.
- **Held, on current evidence:** the cron API is correctly
  customer-scoped (`weft.crons_list` returned only this customer's
  crons); `weft.ls /log` exposed only this customer's namespaces. The
  `WHERE customer = ?` row predicate on log *data* appears intact.
- **Not yet verified:** the data-plane RLS itself, and *who* may read
  `/log/weft/coordinator/*`. If only operators, blast radius is
  "operator sees tenant query predicates" — still a confidentiality
  concern, since the codebase explicitly treats `$CUSTOMER` as a
  security boundary that must never surface. If a regular tenant can
  read the ops namespace, materially worse.

## Relationship to the query-audit split

The shipped split moved `query: … sql=…` lines to
`coordinator/queries.log`, but `plan error` and `ingest-stream` lines
still land in `coordinator/main.log` carrying foreign-tenant pipeline
strings and `customer=` values. The remediation is the same shape as
that split, extended to those two remaining emit sites.

## Next steps (ordered — do not probe before 1)

1. **Establish who can read `/log/weft/*`.** Read the
   namespace-authorization code (`api` / `auth` / `coordinator`
   namespace checks). Pure investigation, no probing. This sets the
   severity and is the gating question.
2. **One controlled, read-only RLS probe.** As this customer, attempt
   to read another tenant's pseudofile and confirm it returns nothing /
   denied — distinguishes "logging leak only" from "RLS broken."
   Authorized testing of our own system, but a deliberate cross-tenant
   access attempt: gate on explicit operator approval before running.
3. **Remediate.** Restrict `/log/weft/coordinator/*` readership to
   operators, and/or scrub foreign-tenant identifiers (other customers'
   names, raw SQL / pipeline text) from the lines that reach
   `coordinator/main.log` — the audit-split-shaped fix applied to the
   `plan error` and `ingest-stream` emit sites.

## Status

🔴 Found, untriaged. Step 1 is pure code reading and unblocks the
severity call; nothing here is scheduled until that answer exists.
