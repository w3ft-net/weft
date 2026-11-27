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

## Documentation

- `docs/architecture.md`: system overview
- `docs/query-execution.md`: predicate pushdown and shard routing
- `docs/shard-lifecycle.md`: shard states and window rotation
- `docs/ingestion-*.md`: CloudWatch, weftd and syslog ingest
- `docs/manual/`, `docs/shell/`: the user manual and the shell's help corpus

## License

MIT. See `LICENSE`.
