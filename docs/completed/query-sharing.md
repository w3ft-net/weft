# Plan: Query sharing

Two related features on top of the existing `SavedQuery` model: team-visible saved queries, and per-query URLs that a signed-in teammate can open to land in the exact same shell state.

## Why add this

`store.SavedQuery` today keys on `UserID` — a query I save is invisible to everyone else on my team, even if we're investigating the same incident. The first workaround customers reach for is paste-the-whole-pipeline-into-Slack, which loses the time range and breaks quoting. The second is "did you bookmark the shell URL?", except the shell URL doesn't encode the pipeline either.

Both are solvable with small additions. Public/unauth share URLs are out of scope — they open an attack surface (scraping, link leaks to non-members) that deserves its own plan if a customer names it as a requirement.

## Architecture

### Team-visible saved queries

Extend `SavedQuery` with `scope` ∈ `user | team` and `customer_id`:

```
SavedQuery {
  ID       string
  UserID   string      # original creator
  CustomerID string    # NEW — for team-scoped visibility
  Scope    string      # NEW — "user" or "team"
  Name     string
  Query    string
}
```

List queries logic:

- `user` scope → visible only to `UserID`.
- `team` scope → visible to every active `TeamMember` of `CustomerID`.

No ACL beyond that. Anyone on the team can read, edit, or delete a team-scoped query; removing a user from the team removes their access. Matches the "everyone on the team sees the same /tmp" model `docs/manual/tmp.md` already sets.

### Share URLs

A share URL captures enough state to reproduce the view:

```
https://w3ft.net/shell?q=<savedQueryID>&range=6h..
https://w3ft.net/shell?pipeline=<base64url>&range=6h..
```

Two forms:

- **By saved-query ID** (`q=...`): requires the user opening the link to be a team member (or the query to be user-scoped and belong to them). The range comes from the URL, which lets the same saved query open at different windows without duplicating the saved entry.
- **Ephemeral** (`pipeline=...`): base64url-encoded pipeline string, no server-side state. Any signed-in user on any customer can open it — opening it doesn't grant access to anyone else's logs, because the shell's customer scoping still applies. This is the "send a teammate a URL you just typed into the shell" case.

Both require a signed-in user; neither form exposes data to unauthenticated visitors.

## Components to build

1. **`store.SavedQuery`** — migration adds `customer_id` and `scope`. Backfill: existing rows get `scope=user` and `customer_id` resolved via `TeamMember(UserID)`. Unique index on `(customer_id, scope, name)` for team scope so two team members can't both create a team-scoped "errors" query.
2. **API** (`api/handlers.go`) — `POST /api/queries` accepts `scope` param; `GET /api/queries` returns union of (my user-scoped) + (my team-scoped). `DELETE` respects scope.
3. **Frontend — Shell page**:
   - Left sidebar adds a "Team" section under the existing "Saved" list.
   - "Save query" dialog has a radio: `Just for me` / `Whole team`.
   - "Share" button on the current command line copies a URL to clipboard; modal offers ephemeral vs. (if saved) saved-query link.
4. **Shell URL handling** — `Shell.tsx` reads `?q=` / `?pipeline=` / `?range=` on mount, hydrates the input and range, submits on load or waits for Enter (toggleable).
5. **Docs** — short addition to `docs/manual/query-logs.md` pointing at the team-queries sidebar; new `docs/manual/sharing.md` for the URL mechanics.

## Estimated effort

- Store migration + model change + backfill: **0.5 day**
- API handlers: **0.5 day**
- Sidebar split + save-with-scope dialog: **0.75 day**
- Share-URL modal + clipboard write: **0.5 day**
- URL parse + hydrate in Shell.tsx: **0.5 day**
- Docs + tests: **0.5 day**

**Total: ~2.5 days** for team-visible + signed-in-share-URLs.

## Non-goals

- **No public / unauthenticated links.** A shared URL always requires a signed-in user on a matching customer (or any customer, for ephemeral). If a customer explicitly wants "paste this on Twitter and anyone can view the query result," that's a separate plan with distinct security review.
- **No edit lock / collaborative editing.** Team-scoped queries are last-write-wins, same as `/bin` scripts today.
- **No per-query RBAC.** Namespace permissions already gate whether a user can *run* a query against a namespace they don't have access to; we don't need a second ACL layer on the bookmark.
- **No time-limited share URLs.** Signed-in-only is the access control; no separate expiry. Add expiry when public links do.
- **No share-to-Slack quick action.** A teammate copying a URL into Slack is fine; we don't need to bake the integration.
