# Trust model

## Who talks to whom

| Link | Protocol | Credential |
|---|---|---|
| browser or CLI to api | HTTP | the identity's (Basic, a session token) |
| api to coordinator | gRPC | the deployment's gRPC identity |
| coordinator to compute | gRPC | none; compute is internal |
| coordinator and compute to shards | gRPC | `SHARD_SECRET` bearer token |
| coordinator to shards, lifecycle | HTTP (`/health`, `/transition`, `/shutdown`) | none |
| shards to coordinator | gRPC heartbeats | `SHARD_SECRET` |
| weftd to coordinator | gRPC | the ingest token |

Only the api's HTTP port and the coordinator's ingest port are meant to be
reachable from outside. Everything else assumes a private network: compute, the
shards, the coordinator's local and internal ports, the health ports, the
localhost-only admin and pprof ports. Keep them off the internet; anything that
can reach a shard's lifecycle HTTP can shut it down, and anything holding
`SHARD_SECRET` can read every shard.

## TLS

By default the api serves plain HTTP and the gRPC links are plaintext. Put a
TLS-terminating proxy in front of the api, or give every process the same
settings (flags, or the `WEFT_TLS_*` environment):

- `--tls-cert` and `--tls-key`: serve TLS (the api's HTTP port, every gRPC
  listener). Set on every process that is dialed.
- `--tls-ca`: dial peers over TLS, trusting the certificates this PEM bundle
  signs. Set on every process that dials.

Both ends must agree: a plaintext client cannot reach a TLS server. The shard
lifecycle HTTP and the health ports stay plaintext either way. weftd's own link
to the coordinator is configured in weftd (`endpoint:`, `tls:`).

## The Docker launcher

The Docker launcher starts shards as sibling containers through the Docker
socket. A coordinator with `/var/run/docker.sock` mounted can run any container
on the host, which is root on that host. Run it only where the coordinator is
trusted as such, and never expose its ports. The example compose file mounts the
socket for this reason alone.

## Secrets

Notify secrets are encrypted at rest with AES-GCM when `WEFT_SECRETS_KEY` is
set; without it they are stored in clear and the process warns at startup.
`SHARD_SECRET` is read from the environment of each process and is never
stored. Tokens are stored hashed.
