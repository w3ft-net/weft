# weft

A multi-tenant log analysis service. Logs are ingested into time-sharded
DuckDB and Parquet nodes and queried through a shell that presents log sources
as files and runs Unix pipeline idioms (grep, awk, sed, jq) over them. The query
planner pushes filters down into shard-level SQL and runs the rest on a pool of
stateless compute workers.

## Processes

Core runs as three processes that talk gRPC, on one machine or several:

- **api**: the HTTP API and, when built with `-tags webembed`, the web app.
- **coordinator**: parses pipelines, plans and routes queries, owns the shard
  lifecycle, and runs ingest.
- **compute**: stateless workers for the pipeline stages that cannot be pushed
  down.

Shards are launched by the coordinator. `weftd` is an optional agent for
customer hosts that ships log lines to the coordinator. `weft` is the CLI.

## Build and test

    make test            # go test -tags test ./...
    make web             # build the web app (needs node)
    make api-web         # the api with the web app embedded

`docker-compose.yml` brings up a single-machine stack that the end-to-end tests
in `e2e/` run against:

    docker compose up -d --build
    go test -tags e2e ./e2e/ -v

The module depends on `warp` and `warp-lua`; see `go.mod`.

## Runtime settings

Rate limits, body caps, timeouts and version floors are flat `UPPER_SNAKE: value`
pairs (the keys are in `config/runtime.go`). They come from the YAML file at
`WEFT_CONFIG_FILE` (default `/etc/weft/config.yaml`; absent is fine), overridden
by environment variables of the same name. They reload every minute and on
SIGHUP. `WEFT_CONFIG_SOURCE=ssm` reads an AWS SSM parameter instead of the file.

## Sign-in

The default identity is one HTTP Basic credential (`WEFT_BASIC_AUTH_USER` and
`WEFT_BASIC_AUTH_PASSWORD`). With `WEFT_AUTH=magiclink` people sign in with an
emailed link instead: set `WEFT_SESSION_SECRET` (32+ characters, the same on the
api and the coordinator) and SMTP (`EMAIL_HOST`, `EMAIL_HOST_PASSWORD`,
`EMAIL_HOST_USER`, `EMAIL_FROM`). Who may sign in is the `login_emails` table;
add an address with SQL until there is a command:

    sqlite3 weft.db "INSERT INTO login_emails (email, created_at) VALUES ('you@example.com', datetime('now'))"

## Secrets

Notify secrets (webhook tokens and the like) are encrypted at rest with AES-GCM
when `WEFT_SECRETS_KEY` is set; without it they are stored in clear and the
process warns at startup. Keep the key: a ciphertext does not read without it.

## Documentation

- `docs/architecture.md`: system overview
- `docs/query-execution.md`: predicate pushdown and shard routing
- `docs/security.md`: the trust model, TLS and secrets
- `docs/shard-lifecycle.md`: shard states and window rotation
- `docs/ingestion-*.md`: CloudWatch, weftd and syslog ingest
- `docs/manual/`, `docs/shell/`: the user manual and the shell's help corpus

## License

MIT. See `LICENSE`.
