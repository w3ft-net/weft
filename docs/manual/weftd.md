---
id: manual-weftd
title: Install weftd
web:
  section: install-weftd
  category: connect-source
  order: 30
---

## Install weftd

weftd is a lightweight Go binary that tails log files and ships structured
lines to weft over gRPC. Download it from the **Downloads** page.

```
# Download and install
curl -fsSL https://w3ft.net/install.sh | sh

# Or on Debian/Ubuntu
sudo dpkg -i weftd_*.deb
```

Configure your log sources in `/etc/weftd/config.yaml`:

```
# /etc/weftd/config.yaml

endpoint: ingest.w3ft.net:9020 # optional; defaults to this. Override
                              # only for local/dockerized testing
token: wft_a1b2c3d4e5f6...
deployment: api               # optional; the fleet/scaling-unit
                              # this agent belongs to (k8s Deployment,
                              # AWS ASG, etc.) — stamped on every row
region: us-east-2             # optional override; auto-detected from
                              # cloud metadata at startup if omitted
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
    path: /var/log/myapp/*.log
    schema: prefix_json
    prefix_fields: [ts, level, service]
    prefix_separator: " "
    json_field: payload
    version:                           # optional
      file: /opt/myapp/VERSION         # weftd watches the file and
                                       # stamps the latest contents as
                                       # VERSION on every row from this
                                       # source (or use `env:` to read
                                       # a variable once at startup)

  - service: nginx
    path: /var/log/nginx/access.log
    schema: json
```

This file is the single source of truth — the package never
generates or overwrites it. Start from `/etc/weftd/config.yaml.example`.
Repeated source fields can be DRYed with standard YAML anchors and
merge keys (`&name` / `<<: *name`); weftd ignores an `x-anchors:`
holder block, so it's a convenient place to declare them.

### Service names

Each source's `service:` sets the `<service>` segment of its
pseudofile path. It is optional: for a systemd source (`unit:`) weftd
uses the unit name without `.service`, and for a file source (`path:`)
it uses the file's name without its extension, so
`/var/log/myapp/api.log` becomes `api`. Set `service:` explicitly when
several files should share one service, or when the derived name isn't
the one you want. This is separate from the top-level `services:` list
above, which only controls how host metrics fan out.

### Token

Put `token:` in the config file, **or** supply it via the
`WEFTD_TOKEN` environment variable (e.g. in `/etc/default/weftd`).
The environment wins over the file value — an AWS-shaped credential
chain — so a secret injector can provide the token without templating
the YAML. It's the only setting with an environment source; everything
structural lives in the file.

The `deployment`, `region`, and per-source `version` values
become first-class query dimensions — see the **Meta-variables**
section in **Query your logs** for the full list and how they
appear in awk and jq.

### File-sourced dimensions

Each entry under `dimensions:` is `<column>: <path>` or
`<column>: <path>:.<json.path>`. The reader stamps the resulting
value on every row weftd ships from this host. Operator config
wins over auto-derived sources (e.g. `dimensions.region` overrides
the cloud-metadata probe). Allowed columns: `deployment`,
`version`, `region`, `cluster`, `node`. Other columns are
populated by other mechanisms (token-bound env, schema-extracted
fields, k8s log-path metadata) and refusing to override them
keeps those guarantees intact.

The JSON-path syntax is a minimal subset of jq — bare field
access (`.foo`), nested fields (`.foo.bar`), and array indices
(`.items[0].name`). For richer extraction, pre-process the file
with your own tooling and point weftd at the result.

### Schema types

- `json` — pure JSON lines, no prefix
- `prefix_json` — fixed-width prefix fields followed by a JSON payload
- `raw` — unstructured text, no parsing

```
# Start weftd
sudo systemctl enable --now weftd

# Check status
sudo systemctl status weftd
journalctl -u weftd -f
```

### Resuming after a restart

weftd remembers how far it has delivered each tailed file, so a restart
(an upgrade, a reboot, a crash) picks up where it left off instead of
skipping whatever was written in the meantime. The saved position means
"the coordinator acknowledged everything up to here", not "weftd read up to
here", so lines that were in flight when weftd stopped are sent again rather
than lost. Delivery is at-least-once: after an unclean stop you may see a few
seconds of lines twice, never a gap.

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

- **The first restart onto a version with checkpointing still loses lines in
  flight**, because the old binary does the shutdown. The guarantee starts
  from the second restart.
- **Containers:** the state directory must survive the pod. On Kubernetes,
  mount `/var/lib/weftd` from a `hostPath`; an `emptyDir` or the container
  filesystem is wiped on every reschedule, which is exactly when you need it.
- **Two weftd on one host** cannot share a state directory. The second one
  logs that the directory is locked and runs without checkpointing.
- A damaged `state.json` is moved to `state.json.corrupt` and weftd carries
  on as if freshly installed; it never refuses to start.

### System metrics (optional)

weftd can also ship host metrics alongside log lines. Opt in by
attaching a `metrics:` block to any source in the config — the block
is independent of what that source tails, it just rides along on the
same agent process.

```
# /etc/weftd/config.yaml
endpoint: ingest.w3ft.net:9020
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
rows. The top-level `services:` list controls the destination paths:
each entry produces its own `/log/<namespace>/<service>/metrics.log`
stream. Co-located services (e.g., `services: [coordinator, api]`
on a host that runs both) get byte-identical rows under each path,
filterable by the `host` field when the ASG scales out.

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

Beyond tailing files, weftd exposes a local records socket that
running applications can write to directly. It binds to a Unix
socket by default, so process-local apps can ship to weft without
a token, network call, or extra dependency — events, heartbeats,
and runtime stats all ride this one channel. There is a typed Go
SDK at <https://github.com/w3ft-net/weft-go> for callers that
prefer not to wire up the socket by hand.

#### Records socket

Add a `socket:` line to `config.yaml` and weftd will accept
newline-delimited frames in the format `<app>:<line>\n`. Each
line is forwarded to the same ingest path file tailers use,
stamped with whichever `<app>` prefix you write before the colon.

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

A heartbeat is just a record. The Go SDK's `Heartbeat` loop emits one
to app `<service>/heartbeats` every interval over the **records socket
above** — there is no separate heartbeat socket or weftd listener.
Each beat lands at `/log/<namespace>/<service>/heartbeats.log`:

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

Liveness is judged by **recency**: a service that's alive keeps
emitting rows; one that stops is *absent*. Since each beat is emitted
at the source when the process is healthy, there's no server-computed
`age_s` — you ask "have we seen a beat lately?" instead, which a cron
expresses directly. First a cron whose result is the beats in its window
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

Don't add `-n` or `head`/`tail` counts to the first cron: crons are
bound to their time range, and a line count can silently truncate it.

#### Go SDK

`github.com/w3ft-net/weft-go` is the typed front door for the
records socket above — events, heartbeats, and a runtime-stats
collector that periodically emits Go heap, GC pause, goroutine, and
FD-count telemetry. Setup in `main()`:

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
