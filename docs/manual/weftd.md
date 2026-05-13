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

endpoint: w3ft.net:443
token: wft_a1b2c3d4e5f6...
deployment: api               # optional; the fleet/scaling-unit
                              # this agent belongs to (k8s Deployment,
                              # AWS ASG, etc.) — stamped on every row
region: us-east-2             # optional override; auto-detected from
                              # cloud metadata at startup if omitted
services: [api]               # logical services co-located on this
                              # host; metrics and heartbeat snapshots
                              # fan out one stream per service —
                              # /log/<ns>/<service>/{metrics,heartbeats}.log
dimensions:                   # optional: column values sourced from
                              # files on disk and watched live via
                              # fsnotify, so a deploy that touches
                              # the file flows through to the next
                              # row without an agent restart
  node: /etc/weft/identity.json:.shard_id

sources:
  - app: myapp
    path: /var/log/myapp/*.log
    schema: prefix_json
    prefix_fields: [ts, level, service]
    prefix_separator: " "
    json_field: payload
    version_file: /opt/myapp/VERSION   # optional; weftd watches the
                                       # file and stamps the latest
                                       # contents as $VERSION on
                                       # every row from this source

  - app: nginx
    path: /var/log/nginx/access.log
    schema: json
```

The `deployment`, `region`, and per-source `version_file` values
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

### System metrics (optional)

weftd can also ship host metrics alongside log lines. Opt in by
attaching a `metrics:` block to any source in the config — the block
is independent of what that source tails, it just rides along on the
same agent process.

```
# /etc/weftd/config.yaml
endpoint: w3ft.net:443
token: wft_a1b2c3d4e5f6...
deployment: api               # optional; tags every row

sources:
  - app: myapp
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
tail /log/myns/metrics.log | jq 'select(.type == "memory") | .pct'

# Per-host disk usage, alerting above 80%
tail /log/myns/metrics.log \
  | jq -r 'select(.type == "disk") | "\(.host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops

# Recent load averages by host
tail -n 1000 /log/myns/metrics.log \
  | jq 'select(.type == "load") | {host, load5}'
```

Metrics only run on Linux (the collectors read `/proc/stat`,
`/proc/meminfo`, `/proc/mounts`, `/proc/loadavg`, and
`/proc/net/dev`). On other kernels the block is ignored.

### App API: emit events and heartbeats from your services

Beyond tailing files, weftd exposes two local endpoints that
running applications can talk to directly. Both bind to Unix
sockets by default, so process-local apps can ship to weft without
a token, network call, or extra dependency. There is a typed Go
SDK at <https://github.com/w3ft-net/weft-go> for callers that
prefer not to wire up sockets by hand.

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

#### Heartbeat endpoint

weftd also runs a tiny heartbeat listener that records "this
service is alive" pings and emits the table as a periodic
snapshot to a pseudofile at `/log/<namespace>/heartbeats.log` —
absence-of-heartbeat becomes a regular weft query. Anything that
speaks HTTP can hit it; the SDK wraps the wire shape in one line.

```
# /etc/weftd/config.yaml
heartbeat:
  socket: /var/run/weftd-heartbeat.sock   # default; "" disables
  port: 14001                              # optional; 0 disables (default)
  snapshot_interval: 5s                    # default
  stale_after: 60s                         # drop services silent past this
```

Every POST to `/h/<service>` registers a heartbeat for that
service. Empty body, 204 on success.

```
# From cron, shell scripts, k8s liveness probes
curl -X POST --unix-socket /var/run/weftd-heartbeat.sock \
  http://localhost/h/my-service

# Or over the optional TCP listener
curl -X POST http://127.0.0.1:14001/h/my-service

# From Go via the SDK — one line in main()
go c.HeartbeatLoop(ctx, "my-service", 30*time.Second)
```

Service names are restricted to `[a-zA-Z0-9._-]`, 1–128 chars.

Each tick of the snapshot ticker (default 5s) emits one row per
known service to its own `/log/<namespace>/<service>/heartbeats.log`
stream — the per-service path makes "is X alive right now?" a
direct `tail`, no filtering required:

```
{"type":"heartbeat","ts":"2026-04-29T14:55:36Z",
 "service":"my-service","last_seen":"2026-04-29T14:55:35.4Z","age_s":0.6}
```

A service silent past `stale_after` is dropped from subsequent
snapshots, so the live set is always self-cleaning. The `age_s`
field grows from 0 toward `stale_after` as a service goes quiet —
catch a stalling service before it disappears with a normal
alert pipeline:

```
# Alert when billing hasn't checked in for 30s+
tail -n 1 /log/myns/billing/heartbeats.log \
  | jq 'select(.age_s > 30)' \
  | alert above 0 -t 1 --notify=ops
```

#### Go SDK

`github.com/w3ft-net/weft-go` is the typed front door for both
endpoints above plus a runtime-stats collector that periodically
emits Go heap, GC pause, goroutine, and FD-count telemetry.
Three-line setup in `main()`:

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
