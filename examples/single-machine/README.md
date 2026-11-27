# single-machine

The open weft stack on one machine: api, coordinator and compute in containers,
with shards launched by the coordinator as sibling containers through the Docker
socket. One group, one HTTP Basic credential, SQLite for every store, git for
tracked files.

It composes core with `ext/backends/sqlite`, `ext/identity/basic` (or
`ext/identity/magiclink` with `WEFT_AUTH=magiclink`) and `ext/launcher/docker`.
It imports nothing else, so it carries no AWS requirement.

## Run

The image build copies `warp`, `warp-lua` and `weft-core` from one parent
directory, so check them out as siblings. Then, from this directory:

    docker compose up -d --build

The api is at http://localhost:18080. The compose file sets the credential
(`e2e` / `e2e-password`) and a throwaway shard secret: change both for anything
beyond a trial.

## Test

    go test -tags e2e ./e2e/ -v -timeout=10m

The suite ingests a line through the coordinator's gRPC port and queries it back
through the api, and checks that a request without the credential is refused.

## Layout

- `cmd/{api,coordinator,compute,shard}`: the mains, each a few lines over core's
  `cmd/*/server` or `cmd/*/Run`.
- `local/`: the deployment the api and coordinator open (SQLite, git history,
  identity chosen by `WEFT_AUTH`).
- `deploy/entrypoint.sh`: selects the process by `ROLE`.
