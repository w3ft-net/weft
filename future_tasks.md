# Future tasks

## Stale unactivated user cleanup
Users are created at invite time (before they accept). If an invite is never accepted, the user record remains with no login activity. Add a periodic job or admin command to clean up user records where:
- The associated TeamMember status is still "pending"
- The invite was created more than 30 days ago
- The user has never logged in (no MagicLink with `used=true` for their email)

## Token management in Namespaces page
Tokens are created per-namespace. The Namespaces page should show tokens grouped by namespace, with create/revoke controls scoped by NamespacePermission.can_create_tokens.

## Namespace permission enforcement in shell queries
The coordinator should filter query results by the user's readable namespaces. Currently only customer-level filtering is applied. Need to pass user context through the query path and inject namespace-level WHERE clauses.
