# Plan: User SSO (OIDC)

Per-customer OIDC login for end users, covering Google, Okta, Azure AD, Auth0, and anything else that speaks OpenID Connect. SAML and SCIM are explicitly out of scope for v1.

## Why add this

Today the only ways into the web UI are magic-link email and the CLI device flow. Both are workable for solo and small-team users; both fall over for any enterprise buyer with central IdP requirements, offboarding policies, or "no personal email logins" rules. OIDC is the minimum credible SSO story and it lands in one library dependency (`github.com/coreos/go-oidc/v3`).

The adjacent `docs/plans/oidc.md` is about GitHub Actions deploy, not user auth — they share no code and no data path.

## Architecture

Per-customer OIDC configuration: a `Customer` optionally has an `OIDCConfig` row with `{issuer_url, client_id, client_secret, allowed_domain?}`. When present, the login page offers an "SSO" button; the magic-link path still works as a fallback per user.

Flow:

```
User visits /login
  → Login.tsx lists "Sign in with SSO (your-company.com)" if a
    hostname or customer-slug match resolves to an OIDCConfig
  → GET /api/auth/oidc/{customer-slug}/start
    → coordinator builds PKCE + state, redirects to issuer /authorize
  → IdP → GET /api/auth/oidc/{customer-slug}/callback?code=...
    → exchange code for id_token, verify signature via JWKS
    → match id_token.email against TeamMember for this customer
        → match & active → issue our JWT (same 30-day token today's
          magic-link path issues)
        → no match but email domain == allowed_domain → auto-join
          as a member in Status=active (optional, owner-opt-in)
        → otherwise → "not a member of this team" error
  → client lands on /dashboard with JWT in localStorage
```

The issued JWT is identical to today's (`auth/jwt.go`); the IdP is only consulted at login time, not on every request. Revocation is a TeamMember status flip, same as today.

## Components to build

1. **`auth/oidc.go`** — thin wrapper over `coreos/go-oidc`. Discovery, PKCE, code exchange, ID-token verify. Holds a per-customer provider cache (providers are safe to reuse).
2. **`store.OIDCConfig`** — new table: `customer_id`, `issuer_url`, `client_id`, `client_secret` (encrypted via `store/secrets.go` if that exists or via an envelope-encryption addition), `allowed_domain`, timestamps. CRUD methods `SaveOIDCConfig` / `GetOIDCConfig` / `DeleteOIDCConfig`.
3. **API handlers** in `api/handlers.go`:
   - `GET /api/auth/oidc/{slug}/start` — issue state cookie, redirect to IdP.
   - `GET /api/auth/oidc/{slug}/callback` — code exchange, TeamMember resolve, JWT issuance. Same JWT shape as the magic-link path.
   - `GET/PUT /api/customers/{id}/oidc-config` (owner-only) — customer-scoped CRUD.
4. **Frontend**:
   - Account → SSO panel. Form for issuer URL + client ID + secret + allowed domain. "Test connection" button that probes the discovery endpoint and surfaces misconfig (unreachable / invalid JSON / missing `authorization_endpoint`) before saving.
   - Login.tsx: if `/api/auth/oidc/resolve?hint=<email-domain-or-slug>` returns a config, show the SSO button.
5. **Docs** — `docs/manual/sso.md` with per-IdP recipes (Google Workspace, Okta, Azure AD, Auth0). Each recipe is ~15 lines: create OIDC app at IdP, paste redirect URI `https://w3ft.net/api/auth/oidc/<slug>/callback`, copy client ID/secret into weft.
6. **Tier gating** — SSO feature-flagged per plan (probably Yard and above). `user.Tier.HasSSO()` check at the save-config endpoint.
7. **Integration test** — stand up a tiny OIDC provider (there are Go fakes — `github.com/cloudentity/oauth2c` or a hand-rolled 50-line test server) and run a full round-trip.

## Security notes

- **State + PKCE mandatory.** No implicit flow, no "state optional" tolerance.
- **`allowed_domain` is opt-in.** By default, SSO authenticates existing team members only; without the auto-join, a stolen id_token from someone not on the team still can't log in. Auto-join is for teams that want "anyone at the company".
- **Client secret encryption.** We currently store Stripe keys and similar in Postgres — reuse whatever that mechanism is; if none exists, a small envelope encryption against a KMS key is a ~50-line add and worth doing anyway.
- **Revocation.** Flipping TeamMember.Status to `removed` must terminate active JWTs. Today JWTs are stateless and checked against TeamMember on every request (confirm this — if not, add a per-request membership check or move to short-lived access + refresh).

## Estimated effort

- OIDC library wrap + flow handlers: **1 day**
- Config CRUD + encryption path: **1 day**
- UI panel + login-page SSO button: **1 day**
- Allowed-domain auto-join + tier gate: **0.5 day**
- Docs + per-IdP recipes: **0.5 day**
- Integration test with fake IdP: **0.5 day**

**Total: ~4-4.5 days** for a shippable v1.

## Non-goals

- **No SAML.** Single dependency (OIDC library) covers the top four IdPs. SAML is a separate plan once a concrete customer asks for it.
- **No SCIM.** Manual team invite remains the provisioning path. Revisit when a buyer names it as a requirement.
- **No enforced-SSO-only mode.** Magic link stays available to every user regardless of customer SSO config. A future "SSO-only" toggle on the customer record is a small add once we have customers running it.
- **No IdP-initiated login.** SP-initiated only — all flows start from `/login` on our side.
