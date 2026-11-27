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
   An ext lists only the requirements it links.
3. **Conformance suites stay in core** (`backends/*/…test`, `logdb` launcher
   suites). An ext runs them against its implementation in its own tests. Core's own
   tests use in-memory fakes, not an ext.
4. **A seam exists before a move.** Each move below says which interface the
   implementation already satisfies, or which one must be cut first.

## Modules

One Go module per implementation, self-contained: `github.com/w3ft-net/weft/ext/<kind>/<impl>`.
A module's `go.mod` lists only what that implementation links, so an example that
composes `ext/launcher/docker` carries no AWS requirement. A `go.work` at the repo
root covers core, every `ext/*/*` and every `examples/*`.

| Module | Replaces (in core today) | Seam |
|---|---|---|
| `ext/launcher/ec2` | `logdb/provisioner_ec2*.go`, `logdb/volumes.go`, `logdb/export_resources.go` | `logdb.ShardLauncher`, `logdb.VolumeManager`, `logdb.Standbys`: exist. **Cut**: `logdb.Start` picks prod or dev by env and calls the EC2/Docker constructors itself. It takes a `logdb.Platform` instead (launcher, volumes, standby launcher, export resources). |
| `ext/launcher/docker` | `logdb/launcher_docker.go`, `logdb/launcher_local.go` | `logdb.ShardLauncher`: exists. |
| `ext/storage/s3` | `backends/dlq/dlqs3`, the S3 half of `logdb/objectdeleter.go`, `shard/export.go`, the S3 reader config | `dlq.Store`, `logdb.ObjectDeleter`: exist. **Cut**: `weft-shard export` writes through an `archive` writer interface, not S3 directly. |
| `ext/storage/lambda` | `shard/cold_shard.go`, `shard/cold_reader.go`, `shard/cold_prune.go`, the Lambda reader in `compute/archive.go` | `archive.Reader`: exists. **Cut**: `compute.NewArchiveRouter` takes AWS credentials; it takes readers. `shard.ColdShard.WithAWSAuth` and SigV4 go with this module. |
| `ext/ingest/cloudwatch` | `ingest/cloudwatch.go` (the polling client) | The push sources (Firehose, Heroku, Render, syslog, weftd) stay in core: they use no SDK. The poller registers a `sources.Producer`. |
| `ext/llm/bedrock` | the Bedrock client in `ingest/llmdetect.go` (prompts and checker stay) | **Cut**: an `llm.Proposer` interface. The wizard runs on heuristics with none registered. |
| `ext/config/ssm` | `config/ssm.go` | `config.Source`: exists. |
| `ext/tailfanout/redis` | `coordinator/tail_fanout_redis.go` | **Cut**: `TailFanout` takes a `Bus` interface (publish, subscribe); in-process is core's default and needs no ext. |
| `ext/identity/basic` | `deployment/local/identity.go`, `backends/defaults/basic.go` | `api.Identity`, `identity.Server`: exist. |
| `ext/identity/magiclink` | `deployment/magiclink` | The same; it needs a `login.Store` from a backends module. |
| `ext/backends/sqlite` | `backends/defaults` (every store), `sqlitedb`, the assembly in `deployment/local` | The `backends/*` contracts: exist. |
| `ext/backends/git` | `backends/history/gitrepo` | `history` contract: exists. |
| `ext/backends/postgres` | nothing in core (hosted's registries, step 23) | The same contracts. |

`coordinator/discovery/asg.go` is not in the table: nothing calls it. Delete it,
and add `ext/discovery/asg` only if an example needs it.

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

- `single-machine`: `ext/backends/sqlite`, `ext/backends/git`, `ext/launcher/docker`, `ext/identity/basic`,
  in-process fan-out. The e2e suite runs here.
- `tour`: single-machine plus the onboarding package, the seed and the producer.
- `ha-aws`: `ext/backends/postgres`, `ext/launcher/ec2`, `ext/storage/s3`, `ext/storage/lambda`,
  `ext/tailfanout/redis`, `ext/config/ssm`, cron claims.

## Order, with the check after each step

Every step leaves core and hosted building and tested.

1. **Cut the seams** (done, 2026-11-27): `logdb.Platform`, readers into
   `NewArchiveRouter`, `ingest.LLM`, `coordinator.TailBus`, `shard.RequestSigner`.
   Every SDK import now sits in a file of its own: `logdb/provisioner_ec2*.go`,
   `volumes.go`, `export_resources.go`, `objectdeleter_s3.go`, `platform_env.go`;
   `shard/export_s3.go`, `lambda_aws.go`; `ingest/cloudwatch.go`, `llm_bedrock.go`;
   `config/ssm.go`; `coordinator/tail_fanout_redis.go`; `backends/dlq/dlqs3`;
   `cmd/compute` (the reader set).
2. **Mains out of core first** (done, 2026-11-27: `examples/single-machine` holds the four mains, Dockerfile, compose and `e2e/`, which passes; hosted builds its own shard and compute mains from core's `cmd/*/server`; `examples/ha-aws` waits for step 22). An ext imports core, so a core `cmd/*` main cannot
   import an ext: the moment `config/ssm` leaves, `cmd/api`, `cmd/coordinator` and
   `cmd/shard` could no longer build. So before any move, make each process a
   library (`cmd/api/server` and `cmd/coordinator/server` already are; `cmd/shard`
   and `cmd/compute` get the same: a `Run(Options)` and a tiny main; done), and create
   `examples/single-machine` and `examples/ha-aws` holding the mains, with the
   composition still reading core's in-tree implementations. Core keeps only
   libraries; `weft` and `weftd` stay (they link no ext). Add the `go.work`.
3. **Move the SDK-heavy implementations one module per commit** (done, 2026-11-27: `ext/config/ssm`, `ext/tailfanout/redis`, `ext/llm/bedrock`, `ext/storage/lambda`, `ext/storage/s3`, `ext/launcher/ec2`; the CloudWatch poller client and the ASG discovery had no callers and were deleted. Core's `go.mod` has no AWS SDK or go-redis), each with the
   example mains that use it switching their import in the same commit:
   `ext/config/ssm`, `ext/tailfanout/redis`, `ext/llm/bedrock`,
   `ext/ingest/cloudwatch`, `ext/storage/lambda`, `ext/storage/s3`,
   `ext/launcher/ec2`. After the last, `go mod tidy` in core drops the AWS SDK and
   go-redis; verify with `go list -deps ./... | grep -E 'aws-sdk|go-redis'` empty.
   Registries that currently name an ext (`config.SourceFromEnv` knows "ssm")
   take the ext's constructor instead.
4. **Move the zero-dependency implementations** (done, 2026-11-27: `ext/backends/sqlite`, `ext/launcher/docker`, `ext/identity/basic` and `ext/identity/magiclink` are moved and pass; `deployment/local` is now `examples/single-machine/local`; the CLI's Basic credential names its env vars itself; core's tests run on `backends/mem`, and its `go.mod` has no `modernc.org/sqlite`. `backends/history/gitrepo` stays in core: it needs no Go dependency and core's `witrepo` tests use it. Left: `go mod tidy` and tags for the ext modules, which cannot resolve until a core tag carries `backends/mem` and `identity/open`) (`ext/backends/*`,
   `ext/launcher/docker`, `ext/identity/*`). Core's `go.mod` loses
   `modernc.org/sqlite`. Conformance tests move with the implementation; the
   suites stay.
5. **`examples/single-machine` stands alone** (done, 2026-11-27: Dockerfile, compose and README are here; `e2e/` passes against the rebuilt stack).

## Risks

- `logdb` is the hardest cut: `start.go`, `shardsm.go` and `provisioner.go` name EC2
  and Docker types directly. Step 1 for it is the largest refactor in the plan.
- Hosted imports `backends/defaults`, `deployment/local` and the `cmd/*/server`
  libraries. Each move renames import paths there; do the rename in the same push
  as the core tag.
- The release tag. Nested modules are tagged `ext/<kind>/<impl>/vX.Y.Z`; core's tag does
  not carry them. Pin examples and hosted by `replace` through `go.work` until the
  first tags exist.
