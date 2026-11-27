# Layout: core as contracts and engine, implementations in `ext/`

Step 17 of the commercial-to-core plan, on paper. Nothing here is built yet. It
fixes the module boundaries so the moves of steps 18 and 19 are mechanical, and so
that a deployment (a skinnier private weft, say) is assembled from `ext/*` modules
without touching core.

## Rules

1. **Core imports no implementation.** Its `go.mod` has no AWS SDK, no go-redis, no
   SQLite driver, no Docker client. DuckDB stays: it is the shard engine.
2. **An `ext` module imports core, never another `ext` module** (an `examples/`
   composition imports many). Shared helpers an ext needs go in core or are copied.
3. **Conformance suites stay in core** (`backends/*/…test`, `logdb` launcher
   suites). An ext runs them against its implementation in its own tests. Core's own
   tests use in-memory fakes, not an ext.
4. **A seam exists before a move.** Each move below says which interface the
   implementation already satisfies, or which one must be cut first.

## Modules

One Go module per kind, one package per implementation, all under the core
repository: `github.com/w3ft-net/weft/ext/<kind>`. A `go.work` at the repo root
covers core, every `ext/*` and every `examples/*`. A consumer that needs only
`ext/launcher/docker` still lists the module's AWS requirements in `go.mod`, but
links none of them; that is the price of few modules, and the cheaper side.

| Module | Packages | Replaces (in core today) | Seam |
|---|---|---|---|
| `ext/launcher` | `ec2`, `docker`, `local` | `logdb/provisioner_ec2*.go`, `logdb/volumes.go`, `logdb/launcher_docker.go`, `logdb/launcher_local.go` | `logdb.ShardLauncher`, `logdb.VolumeManager`, `logdb.Standbys`: exist. **Cut**: `logdb.Start` picks prod or dev by env and calls the EC2/Docker constructors itself. It takes a `logdb.Platform` instead (launcher, volumes, standby launcher, export). |
| `ext/storage` | `s3` (DLQ, object deleter, archive export, cold reader), `lambda` (cold-shard reader and SigV4 signing) | `backends/dlq/dlqs3`, `logdb/objectdeleter.go` (S3 part), `logdb/export_resources.go`, `shard/export.go`, `shard/cold_shard.go`, `shard/cold_reader.go`, `shard/cold_prune.go`, `compute/archive.go` (Lambda reader) | `dlq.Store`, `logdb.ObjectDeleter`, `archive.Reader`: exist. **Cut**: `compute.NewArchiveRouter` takes AWS credentials; it takes readers. `shard.ColdShard.WithAWSAuth` goes with the Lambda reader. |
| `ext/ingest` | `cloudwatch` (the polling client) | `ingest/cloudwatch.go` | The push sources (Firehose, Heroku, Render, syslog, weftd) stay in core: they use no SDK. The poller registers a `sources.Producer`. |
| `ext/llm` | `bedrock` | `ingest/llmdetect.go` (the Bedrock client; its prompts and checker stay) | **Cut**: an `llm.Proposer` interface. The wizard runs on heuristics with none registered. |
| `ext/config` | `ssm` | `config/ssm.go` | `config.Source`: exists. |
| `ext/tailfanout` | `redis` | `coordinator/tail_fanout_redis.go` | **Cut**: `TailFanout` takes a `Bus` interface (publish, subscribe); in-process is core's default and needs no ext. |
| `ext/discovery` | `asg` | `coordinator/discovery/asg.go` | Nothing calls it today. **Delete**, do not move, unless the single-machine or HA example needs it. |
| `ext/identity` | `basic`, `magiclink` | `deployment/local/identity.go`, `backends/defaults/basic.go`, `deployment/magiclink` | `api.Identity`, `identity.Server`: exist. |
| `ext/backends` | `sqlite` (every store), `git` (history), later `postgres` | `backends/defaults`, `sqlitedb`, `backends/history/gitrepo`, `deployment/local` (the assembly) | The `backends/*` contracts: exist. |

`ext/notify` from the plan has nothing to hold: Slack, PagerDuty, webhook and email
are always in core (SMTP and HTTP only).

## What stays in core

The contracts and the engine: `api`, `coordinator`, `compute`, `shard`
(DuckDB, the live store), `logdb` (the FSMs and the provisioner, minus its
platform code), `planner`, `shell`, `shellfs`, `mounts`, `alerting`, `notifycfg`,
`dashboard`, `web`, `weftd`, the CLI, `proto`, `shape`, the conformance suites, and
the `cmd/*` process bodies as libraries (`server.Run`, `boot.Main`). The `cmd/*`
**mains** move out (step 19): a main is a composition, so it belongs to an example.

## Examples

`examples/<name>/` holds a `go.mod`, the mains, a Dockerfile, a compose file and a
README. Each imports core and the `ext/*` it needs, and nothing else.

- `single-machine`: `ext/backends/sqlite`, `ext/launcher/docker`, `ext/identity/basic`,
  in-process fan-out. The e2e suite runs here.
- `tour`: single-machine plus the onboarding package, the seed and the producer.
- `ha-aws`: `ext/backends/postgres`, `ext/launcher/ec2`, `ext/storage/*`,
  `ext/tailfanout/redis`, `ext/config/ssm`, cron claims.

## Order, with the check after each step

Every step leaves core and hosted building and tested.

1. **Cut the seams** that say **Cut** above, with the implementations still in
   core: `logdb.Platform`, readers into `NewArchiveRouter`, `llm.Proposer`,
   `tailfanout.Bus`. Pure refactors: green before any file moves.
2. **Create the modules and `go.work`**, empty. Hosted adds them to its `go.work`.
3. **Move the SDK-heavy implementations** (`ext/launcher/ec2`, `ext/storage`,
   `ext/ingest`, `ext/llm`, `ext/config`, `ext/tailfanout`), one module per commit.
   After the last, `go mod tidy` in core drops the AWS SDK and go-redis; verify with
   `go list -deps ./... | grep -E 'aws-sdk|go-redis'` empty.
4. **Move the zero-dependency implementations** (`ext/backends`, `ext/launcher/docker`,
   `ext/identity`). Core's `go.mod` loses `modernc.org/sqlite`. Conformance tests
   move with the implementation; the suites stay.
5. **`examples/single-machine`**: the mains, Dockerfile and compose; `e2e/` moves
   here and passes. Core no longer ships `cmd/api`, `cmd/coordinator` and friends as
   binaries; `cmd/*/server` stay as libraries.

## Risks

- `logdb` is the hardest cut: `start.go`, `shardsm.go` and `provisioner.go` name EC2
  and Docker types directly. Step 1 for it is the largest refactor in the plan.
- Hosted imports `backends/defaults`, `deployment/local` and the `cmd/*/server`
  libraries. Each move renames import paths there; do the rename in the same push
  as the core tag.
- The release tag. Nested modules are tagged `ext/<kind>/vX.Y.Z`; core's tag does
  not carry them. Pin examples and hosted by `replace` through `go.work` until the
  first tags exist.
