---
id: manual-weftd
title: Install weftd
web:
  section: install-weftd
  category: connect-source
  order: 30
---

## Install weftd

weftd is weft's own log agent: a single Go binary that tails files and
systemd units on a Linux host and streams structured rows to weft over gRPC.
Why run it instead of a general-purpose shipper? A generic agent moves bytes,
and you hand-write the parsing (regexes, grok patterns) in every host's config
and keep those copies in sync. With weftd the schema lives in weft: the agent
samples a new source, you approve the detected schema once, and every host
running that source then extracts timestamp, level and fields the same way at
the edge. Schema changes reach running agents within about a minute, without
touching the hosts. Every row is also stamped with where it came from
(`instance`, `deployment`, `region`, and a `version` that follows a file on
disk), so those become query dimensions. Rows are queryable in about a second,
against the seconds-to-minutes of the CloudWatch path. Delivery is
at-least-once across restarts and log rotation. One agent also carries host
metrics and a local socket for events and heartbeats from your own code, and it
updates itself. It ships only to weft; it is not a general forwarder. If
minute-scale latency is fine and your logs are already in CloudWatch, the
[CloudWatch source](cloudwatch.md) needs nothing installed.

Download weftd from the **Downloads** page. The steps every source shares
(tokens, confirming rows, approving a schema) are in
[Connect a log source](connect-source.md).

```
# Debian/Ubuntu (x86_64; use arm64 in place of amd64 on ARM hosts)
curl -LO https://releases.w3ft.net/weftd/v{{WEFTD_VERSION}}/weftd_{{WEFTD_VERSION}}_amd64.deb
sudo dpkg -i weftd_{{WEFTD_VERSION}}_amd64.deb
```

The package installs the systemd unit and enables it on boot, but does not
start it: weftd refuses to run until you write its config (below).

Configure your log sources in `/etc/weftd/config.yaml`:

```
# /etc/weftd/config.yaml

endpoint: ingest.w3ft.net:9443 # optional; defaults to this (TLS). Override
                              # for local/dockerized testing
tls: true                     # optional; default true. false only for a
                              # plaintext dev coordinator
token: wft_a1b2c3d4e5f6...
auto_update: true             # optional; default true. false pins this
                              # host to the installed version
deployment: api               # optional; the fleet/scaling-unit
                              # this agent belongs to (k8s Deployment,
                              # AWS ASG, etc.) — stamped on every row
region: us-east-2             # optional override; auto-detected from
                              # cloud metadata at startup if omitted
socket: /var/run/weftd.sock   # optional; records socket for your apps
                              # (see "App API" below)
services: [api]               # logical services co-located on this
                              # host; metrics fan out one stream per
                              # service — /log/<ns>/<service>/metrics.log
                              # (heartbeats come from the app via the SDK)
dimensions:                   # optional: column values sourced from
                              # files on disk and watched live via
                              # fsnotify, so a deploy that touches
                              # the file flows through to the next
                              # row without an agent restart
  node: /etc/weft/identity.json:.shard_id

sources:
  - service: myapp
    path: /var/log/myapp/app.log
    version:                           # optional
      file: /opt/myapp/VERSION         # weftd watches the file and
                                       # stamps the latest contents as
                                       # VERSION on every row from this
                                       # source (or use `env:` to read
                                       # a variable once at startup)

  - service: nginx
    path: /var/log/nginx/access.log

  - unit: myapp-worker.service         # a systemd unit, read from the journal
    kind: main
```

This file is the single source of truth; the package never
generates or overwrites it. Start from `/etc/weftd/config.yaml.example`.
Repeated source fields can be shared with standard YAML anchors and
merge keys (`&name` / `<<: *name`). weftd ignores an `x-anchors:`
block, which is the place to declare them.

### Source types

Each entry under `sources:` tails one thing. Set exactly one of `path:` or
`unit:`.

- **A file** (`path: /var/log/myapp/app.log`). The tailer keeps exact byte
  offsets, follows `logrotate` in both styles (rename-and-recreate and
  `copytruncate`), and drains the old file before it switches to the new one.
  A line is shipped only once its newline has been written. A line longer than
  1 MiB is cut at 1 MiB and ends with `…[line truncated]`. CRLF endings are
  removed, blank lines are skipped, and leading and trailing whitespace is
  trimmed from every line, so indentation (a stack trace's leading spaces,
  say) does not survive.
- **A glob** (`path: /var/log/myapp/*.log`). `*`, `?` and `[...]` work as in
  the shell, and `**` matches any number of directories. Files that exist at
  startup are tailed from the saved position. A file created later in the
  directory the pattern starts from is picked up automatically and read from
  its first line. A file created in a deeper directory after startup is
  found at the next restart. If a concrete file is also named by its own
  non-glob source, that explicit source wins and the glob skips it.
- **A systemd unit** (`unit: myapp.service`). weftd runs
  `journalctl -f -u <unit>` and ships new journal lines from the moment it
  starts. Unit sources are not checkpointed (see
  Delivery below), so a restart does not replay the journal, and
  `journalctl` must be on the host.

### Service names

A source's service is the `<service>` segment of its pseudofile path,
`/log/<namespace>/<service>/<kind>.log`.

- **Single file**: `service:` if you set it, otherwise the file name without
  its extension (`/var/log/myapp/api.log` becomes `api`).
- **Unit**: `service:` if set, otherwise the unit name without `.service`.
- **Glob**: always derived per file from its path below the pattern's fixed
  directory, minus the extension. `/var/log/myapp/*.log` yields one service per
  file (`api`, `worker`), and `/var/log/*/access.log` yields `api/access` and
  `web/access`. **`service:` is ignored on a glob source**, so if several
  files should share one service, list them as separate non-glob sources
  with the same `service:`.

`kind:` is the file part of the pseudofile, `main` by default. Give a second
source on the same service its own `kind:` (for example `kind: audit`) to get
a separate `/log/<ns>/<service>/audit.log`. The names `heartbeats`, `metrics`
and `runtime` are used by the telemetry streams below.

The top-level `services:` list is separate: it only controls how host
metrics fan out.

### Token

Create the token as described in
[Connect a log source](namespaces.md#creating-a-token). Put `token:` in the config file, **or** supply it via the
`WEFTD_TOKEN` environment variable (e.g. in `/etc/default/weftd`).
The environment value takes precedence over the file value, so a secret
injector can provide the token without templating the YAML. It is the
only setting with an environment source; all other settings live in the
file.

Every row also carries `instance`, the host's name. `region` comes from
the config when set, otherwise weftd asks the cloud metadata service once at
startup (AWS IMDSv2, then GCP, then Azure, 500 ms each). It stays empty on
bare metal, where you can set `region:` by hand. A source's `version` comes
from `version.file` (re-read whenever the file changes), else `version.env`
(read once at startup), else `dimensions.version`.

The `deployment`, `region`, and per-source `version` values
become first-class query dimensions — see
[Meta-variables](metavars.md) for the full list and how they appear in awk
and jq.

### File-sourced dimensions

Each entry under `dimensions:` is `<column>: <path>` or
`<column>: <path>:.<json.path>`. The reader stamps the resulting
value on every row weftd ships from this host. Operator config
wins over auto-derived sources (e.g. `dimensions.region` overrides
the cloud-metadata probe). Allowed columns: `deployment`,
`version`, `region`, `cluster`, `node`. Other columns are
populated by other mechanisms (token-bound env, schema-extracted
fields, k8s log-path metadata) and cannot be overridden.

The JSON-path syntax is a minimal subset of jq: field
access (`.foo`), nested fields (`.foo.bar`), and array indices
(`.items[0].name`). For richer extraction, pre-process the file
with your own tooling and point weftd at the result.

### How lines are parsed

The schema lives on the weft server and weftd applies it at the edge. When
a source is new, weftd samples 50 lines (waiting for them if the file is
quiet), submits the sample, and checks every 5 seconds until the source is
approved; see **Check schema setup** in
[Connect a log source](connect-source.md). Until then the source ships
nothing. After approval weftd starts at the end of the file, so history that
was already there is not backfilled. A source that is already approved starts
tailing immediately.

With a schema in hand, weftd splits each line into fields and pulls out the
timestamp and level before sending. The formats are:

- `text` — whitespace-delimited fields
- `json` — each line is a JSON object
- `prefix_json` — a few whitespace-delimited fields, then a JSON object
  that starts at a given field number

weftd re-checks every source's schema once a minute and swaps in a changed
one without a restart or a gap. The same check honors a resample request
from `weft source` while the tailer keeps running. Once a minute, for each
source that saw traffic, it logs a `parse stats` line to the journal; a high
`missing-level` percentage there usually means the schema's level field
doesn't match the lines. If a schema can't be compiled, weftd keeps the
previous one, or ships the raw line stamped with the arrival time when it has
none.

```
# Start weftd
sudo systemctl enable --now weftd

# Check status
sudo systemctl status weftd
journalctl -u weftd -f
```

The connection to weft is encrypted (TLS, certificate verified against the
host's system roots), so the host needs current CA certificates and outbound
access to port 9443. A config that still names `ingest.w3ft.net:9020` is sent
to 9443 automatically.

weftd takes two flags: `--config` (default `/etc/weftd/config.yaml`) and
`--version`. On start it connects with the token and logs
`authenticated as customer=… namespace=…`. If the coordinator can't be
reached it retries with backoff up to 30 seconds. A rejected token is fatal. An
inactive subscription makes it retry every 24 hours, and the daily
re-authentication exits the process if the subscription lapses, so systemd
restarts it into that retry loop.

### Delivery

Files are the only source with delivery guarantees. weftd keeps recently sent
rows (up to 8 MiB per file) until the coordinator acknowledges them, at the
`commit_interval` below, and only then advances the saved position. Anything
unacknowledged is resent on a new stream, so delivery is **at-least-once**:
duplicates are possible, gaps are not. During an outage the log file is the
buffer. weftd stops reading when its window is full and resumes when the
coordinator is back, and there is no disk spool of its own. Unit
(`journalctl`), records-socket and metrics rows are best-effort: they are
sent straight through and are not replayed after a restart or an outage.
The socket drops lines when its 4096-line queue is full.

### Resuming after a restart

weftd remembers how far it has delivered each tailed file, so a restart
(an upgrade, a reboot, a crash) picks up where it left off and
reads whatever was written in the meantime. The saved position means
"the coordinator acknowledged everything up to here", so lines that were
in flight when weftd stopped are sent again. Delivery is at-least-once:
after an unclean stop you may see a few seconds of lines twice, with no
gap.

State is kept in `/var/lib/weftd/checkpoints/`. The `.deb` unit creates it
(`StateDirectory=weftd`). Everything is optional:

```yaml
checkpoint:
  dir: /var/lib/weftd/checkpoints   # where state.json lives
  enabled: true                     # set false to tail from the end on every start
  flush_interval: 5s                # how often positions are written to disk
  commit_interval: 15s              # how often delivered lines are acknowledged
  max_resume_bytes: 256MiB          # per-file cap on backlog replayed after downtime
```

What weftd does for each file when it starts:

| Situation | Starts at |
|---|---|
| First start with checkpointing (no state yet) | the end of the file |
| File has no saved position (appeared while weftd was down) | the beginning |
| File matches its saved position | the saved position |
| File is shorter than the saved position (truncated) | the beginning |
| File was replaced while weftd was down (rotated) | the beginning; the old file's unread tail is not recovered |
| More than `max_resume_bytes` behind | the newest `max_resume_bytes`, from a line boundary |

A file is matched by device, inode and a hash of its first kilobyte, so a
reused inode is not mistaken for the same file. Files created while weftd is
running are read from their first line. Every skip or replay is logged, and
summarized once a minute as a `checkpoint stats` line per file.

Notes:

- **The first restart onto a version with checkpointing loses lines in
  flight**, because the old binary does the shutdown. The guarantee starts
  from the second restart.
- **Containers:** the state directory must survive the pod. On Kubernetes,
  mount `/var/lib/weftd` from a `hostPath`. An `emptyDir` or the container
  filesystem is wiped on every reschedule.
- **Two weftd on one host** cannot share a state directory. The second one
  logs that the directory is locked and runs without checkpointing.
- A damaged `state.json` is moved to `state.json.corrupt` and weftd starts
  as if freshly installed.

### Updates

Once a day weftd checks `releases.w3ft.net` for a newer release. By default it
downloads the build for the host's platform, verifies its SHA-256, replaces the
running binary, and exits so systemd (`Restart=always`) relaunches it. If your
config uses something this version is too old to understand, it also tries to
update before giving up. Set `auto_update: false` to pin a host, for example
while you investigate a release. It then only logs a line when a newer version
exists. A host pinned this way, with a config its version can't use, exits
instead of recovering.

### System metrics (optional)

weftd can also ship host metrics alongside log lines. Opt in by
attaching a `metrics:` block to any source in the config. The block
runs in the same agent process whatever the source tails.

```
# /etc/weftd/config.yaml
endpoint: ingest.w3ft.net:9443
token: wft_a1b2c3d4e5f6...
deployment: api               # optional; tags every row

sources:
  - service: myapp
    path: /var/log/myapp/*.log
    metrics:
      enabled: true
      interval: 30s            # default 30s
      collectors:              # default: all five
        - cpu
        - memory
        - disk
        - load
        - network
```

Samples are read from `/proc` (Linux only) and streamed as JSON log
rows. Enable `metrics:` on one source only: each source with it enabled starts
its own collector, so two would emit every row twice. Unknown collector
names are ignored, and an `interval` that doesn't parse as a duration falls
back to 30s. The top-level `services:` list controls the destination paths:
each entry produces its own `/log/<namespace>/<service>/metrics.log`
stream. Co-located services (e.g., `services: [coordinator, api]`
on a host that runs both) get byte-identical rows under each path,
filterable by the `host` field when the ASG scales out. If `services:` is
unset, weftd uses `deployment:` as the single service; if neither is set the
collector logs that and does not run.

Each tick emits one row per metric type, with one extra row per
mount (disk) and per interface (network):

| `type` | Fields |
|---|---|
| `cpu` | `user`, `system`, `idle`, `iowait` (percent), `cores` |
| `memory` | `total_mb`, `used_mb`, `available_mb`, `swap_used_mb`, `pct` |
| `disk` | `path`, `total_gb`, `used_gb`, `pct` — one row per `/dev/*` mount |
| `load` | `load1`, `load5`, `load15` |
| `network` | `iface`, `rx_bytes_sec`, `tx_bytes_sec` — one row per non-loopback interface |

Every row additionally carries `ts`, `host`, and `deployment`. `cpu`
and `network` need two samples to compute a delta, so the first tick
after startup is silent for those two.

Query metrics the same way you query any JSON log source:

```
# Recent memory pressure
tail /log/myns/api/metrics.log | jq 'select(.type == "memory") | .pct'

# Per-host disk usage, alerting above 80%
tail /log/myns/api/metrics.log \
  | jq -r 'select(.type == "disk") | "\(.host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops

# Recent load averages by host
tail -n 1000 /log/myns/api/metrics.log \
  | jq 'select(.type == "load") | {host, load5}'
```

Metrics only run on Linux (the collectors read `/proc/stat`,
`/proc/meminfo`, `/proc/mounts`, `/proc/loadavg`, and
`/proc/net/dev`). On other kernels the block is ignored.

### App API: emit events and heartbeats from your services

weftd exposes a local records socket that running applications can
write to directly. It binds to a Unix socket by default, so
process-local apps ship to weft with no token, network call, or extra
dependency. Events, heartbeats, and runtime stats all use this one
channel. A typed Go SDK is available at
<https://github.com/w3ft-net/weft-go>.

#### Records socket

Add a `socket:` line to `config.yaml` and weftd will accept
newline-delimited frames in the format `<app>:<line>\n`. Each
line is forwarded to the same ingest path file tailers use,
stamped with the `<app>` prefix you write before the colon.
`<app>` is `service` or `service/kind` (a bare `billing` lands in
`/log/<ns>/billing/main.log`). The prefix must be under 32 characters with
no space before the colon. A line without one goes to an app named
`default`, and a line whose first colon happens to fall inside those 32
characters (`{"id":1}`, `info: reset`) is read as having one, so always
write the prefix. Lines may be up to 1 MiB.

weftd creates the socket's directory, replaces a stale socket file, and makes
the socket world-writable (mode 0666) so any local process can write to
it. Treat the host's local users as trusted writers.

```
# /etc/weftd/config.yaml
socket: /var/run/weftd.sock
```

```
# Shell scripts and cron entries — anything that can write to a socket
echo "billing:user signup id=42" | nc -U /var/run/weftd.sock

# Go services via the SDK
import "github.com/w3ft-net/weft-go"

c, _ := weft.New()
defer c.Close()
c.Send("billing", map[string]any{"event": "signup", "id": 42})
```

The SDK looks for the socket at `/var/run/weftd.sock` by default;
override with `WEFT_SOCKET` or `weft.WithSocket(...)`.

#### Heartbeats

A heartbeat is a record. The Go SDK's `Heartbeat` loop emits one
to app `<service>/heartbeats` every interval over the **records socket
above**. Each beat lands at `/log/<namespace>/<service>/heartbeats.log`:

```json
{"type":"heartbeat","service":"my-service","ts":"2026-06-11T14:55:36.4Z"}
```

```
# From Go via the SDK — one line in main()
go c.Heartbeat(ctx, "my-service", weft.DefaultHeartbeatInterval)

# Without the SDK, write the same record over the records socket
echo 'my-service/heartbeats:{"type":"heartbeat","service":"my-service"}' \
  | nc -U /var/run/weftd.sock
```

Service names are restricted to `[a-zA-Z0-9._-]`, 1–128 chars.

Liveness is judged by **recency**: a live service keeps emitting rows;
one that stops is *absent*. Each beat is emitted at the source when the
process is healthy, so the question is "have we seen a beat lately?",
which a cron expresses directly. First a cron whose result is the beats in its window
(it is empty when none arrived):

```
# /cron.d/billing-beat
# schedule: */1 * * * *
cat /log/myns/billing/heartbeats.log
```

Then a cron that alerts when `billing-beat` has produced no result in the
last 90s:

```
# /cron.d/billing-down
# schedule: */1 * * * *
weft::is_absent billing-beat 90s
alert --if-zero "$?" --notify=ops
```

Leave `-n` and `head`/`tail` counts off the first cron: crons are
bound to their time range, and a line count can truncate it.

#### Go SDK

`github.com/w3ft-net/weft-go` is the typed client for the
records socket above. It sends events and heartbeats, and includes a
runtime-stats collector that periodically emits Go heap, GC pause,
goroutine, and FD-count telemetry. Setup in `main()`:

```
import "github.com/w3ft-net/weft-go"

c, _ := weft.New()
defer c.Close()
go c.Heartbeat(ctx, "my-service", weft.DefaultHeartbeatInterval)
go c.RuntimeStats(ctx, "my-service", weft.DefaultRuntimeStatsInterval)
```

The SDK prefers the local Unix socket and falls back to an HTTP
endpoint when configured (`WEFT_ENDPOINT` + `WEFT_TOKEN`). See
the repo's README for the full env-var table and option list.
