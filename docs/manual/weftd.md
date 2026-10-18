---
id: manual-weftd
title: weftd
web:
  section: weftd
  category: connect-source
  order: 30
---

weftd is weft's own log agent.  It tails files and systemd units on a Linux
host and streams structured rows to weft over gRPC. It is the fastest way you
can get your logs into weft, and since weft was designed so we could feel like
we were running `tail -f` right on the host, it's what we recommend you use if
you can.

## Connect a host with weftd

This page covers the weftd-specific parts; for an overview, see [Connect a log
source](connect-source.md).

### 1. Install

Download weftd from the **Downloads** page. Or just use curl:

```
curl -LO https://releases.w3ft.net/weftd/v{{WEFTD_VERSION}}/weftd_{{WEFTD_VERSION}}_amd64.deb
sudo dpkg -i weftd_{{WEFTD_VERSION}}_amd64.deb
```

arm64 packages are also available -- just swap the suffix in the paths above.

The package installs the systemd unit and enables it on boot, but does not
start it: weftd won't run until it's configured.

### 2. Create a token

Create a namespaced token as described in [Connect a log
source](namespaces.md#creating-a-token). Put `token:` in
`/etc/weftd/config.yaml`, or supply it as `WEFTD_TOKEN` (for example in
`/etc/default/weftd`). The environment value wins, so a secret injector can
provide the token without templating the YAML. 

### 3. Configure and start

weftd reads `/etc/weftd/config.yaml`. A minimal config needs only a token and
at least one source:

```yaml
# /etc/weftd/config.yaml
token: wft_a1b2c3d4e5f6...

sources:
  - service: myapp
    path: /var/log/myapp/app.log
```

Then start it with `sudo systemctl start weftd`. Rows arrive within a few
seconds. weftd requires a schema, so [approve
one](connect-source.md#sampling-logs-for-a-schema) once the source has
sampled.

## Finding your logs

Weft maps weftd sources to log files as follows:

| weftd source | weft file |
|---|---|
| `path: /var/log/myapp/api.log` | `/log/<ns>/api/main.log` |
| `path: /var/log/myapp/api.log`, `service: myapp` | `/log/<ns>/myapp/main.log` |
| `path: /var/log/myapp/audit.log`, `service: myapp`, `kind: audit` | `/log/<ns>/myapp/audit.log` |
| `unit: myapp-worker.service` | `/log/<ns>/myapp-worker/main.log` |
| `path: /var/log/myapp/*.log` | `/log/<ns>/api/main.log`, `/log/<ns>/worker/main.log`, one per file |
| `path: /var/log/*/access.log` | `/log/<ns>/api/access/main.log`, `/log/<ns>/web/access/main.log` |
| `deployment: web` with a [`metrics:`](#system-metrics-optional) block | `/log/<ns>/web/host.log`, `disk.log` and `network.log` |

Every row carries `instance`, the host's name, so a single
`tail /log/<ns>/myapp/main.log` covers every host.

`service:` is ignored on a glob; the service is derived per file from its path
below the pattern's fixed directory, minus the extension. To make several
files share one service, list them as separate non-glob sources with the same
`service:`. 

(The presence of the `<env>` segment depends on how you created your token.
See [Paths](query-logs.md#paths) for the layout rules.)

## Configuration

General, box-level settings are top-level keys:

| Key | Definition | Default |
|---|---|---|
| `token` | Ingest token for your namespace. May instead come from the `WEFTD_TOKEN` environment variable (see [Connect a host](#connect-a-host-with-weftd)). | none; required |
| `auto_update` | Self-update from `releases.w3ft.net` (see [Updates](#updates)). `false` pins this host to the installed version. | `true` |
| `deployment` | The fleet or scaling unit this agent belongs to (k8s Deployment, AWS ASG, etc.). Stamped on every row, and the service [host metrics](#system-metrics-optional) are written under. | empty |
| `region` | Region stamped on every row. | auto-detected from cloud metadata at startup |
| `socket` | Path of the records socket for your apps (see [App API](#app-api-emit-events-and-heartbeats-from-your-services)). | none; socket disabled |
| `metrics` | Host metrics: cpu, memory, disk, load, network. Having the block turns them on (see [System metrics](#system-metrics-optional)). | no block: off |
| `metavariables` | Values for the `deployment`, `version`, `region`, `cluster` and `node` columns, each read from the environment, a file or a JSON file (see [Metavariables](#metavariables)). Example: `version: {file: /opt/myapp/VERSION}` | none |
| `checkpoint` | Tuning for resume-after-restart, which is always on (see [Resuming after a restart](#resuming-after-a-restart)). | see that section |
| `sources` | The list of things to tail. | none; required |

### Sources

A host can have any number of sources, listed under the `sources` key. Each
entry tails one thing. Set exactly one of `path` or `unit`.

```yaml
sources:
  - service: myapp
    path: /var/log/myapp/app.log
    version:
      file: /opt/myapp/VERSION

  - service: nginx
    path: /var/log/nginx/access.log

  - unit: myapp-worker.service
    kind: main
```

| Key | Definition | Default |
|---|---|---|
| `path` | A file or glob to tail. Mutually exclusive with `unit`. | none |
| `unit` | A systemd unit to read from the journal. Mutually exclusive with `path`. | none |
| `service` | The `<service>` segment of the source's file path in weft (see [Finding your logs](#finding-your-logs)). Ignored on a glob. | file name without extension, or unit name without `.service` |
| `kind` | The file part of the pseudofile, `/log/<ns>/<service>/<kind>.log`. `heartbeats`, `metrics` and `runtime` are reserved. | `main` |
| `metavariables` | This source's own values for `deployment`, `version`, `region`, `cluster` and `node`, overriding the host-wide ones (see [Metavariables](#metavariables)). | none |

Every source, whatever its kind, handles lines the same way. CRLF endings are
removed, blank lines are skipped, and leading and trailing whitespace is
trimmed from every line, so indentation (a stack trace's leading spaces, say)
does not survive. A line longer than 1 MiB is cut at 1 MiB and ends with
`…[line truncated]`.

Some behaviors vary by source type:

- For **concrete files** (`path: /var/log/myapp/app.log`), the tailer keeps exact byte
  offsets, follows `logrotate` in both styles (rename-and-recreate and
  `copytruncate`), and drains the old file before it switches to the new one.
  A line is shipped only once its newline has been written.
- For **glob patterns** (`path: /var/log/myapp/*.log`), `*`, `?` and `[...]` work as in
  the shell, and `**` matches any number of directories. Files that exist at
  startup are tailed from the saved position. A file created later in the
  directory the pattern starts from is picked up automatically and read from
  its first line. A file created in a deeper directory after startup is
  found at the next restart. If a concrete file is also named by its own
  non-glob source, that explicit source wins and the glob skips it. Each
  matched file behaves as a single file does.
- For **systemd unit** (`unit: myapp.service`), weftd runs
  `journalctl -f -u <unit>` and ships new journal lines from the moment it
  starts. Unit sources are not checkpointed (see
  Delivery below), so a restart does not replay the journal, and
  `journalctl` must be on the host.

### Metavariables

Weft refers to dimensions as [metavariables](metavars.md) because of the
first-class support they have in our awk and jq implementations. Using weftd
enables you to set more of them than via other producers. The configuration for
this lives in weftd.yaml.

`region` comes from a top-level config key when set. When not set, weftd asks
the cloud metadata service once at startup when running on AWS, GCP, or Azure. Another set of metavariables (`deployment`, `version`, `region`, `cluster` and
`node`) can be set for the whole host, or for a single source, via one of three
mechanisms: 

| Key | Behavior | Refreshed |
|---|---|---|
| `env` | weftd reads the supplied environment variable | Once, at startup. Changing it needs a weftd restart. |
| `file` | weftd reads the contents of the file and trims whitespace | Whenever the file changes. |
| `json` | weftd reads JSON from a file and extract a value at the supplied json path | Whenever the file changes. |

Examples:

```yaml
metavariables:
  version:
    file: /opt/myapp/VERSION          # your deploy writes e.g. "2026.10.18-3"
  node:
    json: /opt/app/data/shard.json:.shard_id
  cluster:
    env: CLUSTER_NAME
```

A `json:` path is a minimal subset of jq: field access (`.foo`), nested
fields (`.foo.bar`) and array indices (`.items[0].name`). For anything
richer, pre-process the file with your own tooling and point weftd at the
result.

If you have metavariables defined at both the host level and for individual
sources, the source values will win. Rows that belong to no source, such as
host metrics, only ever get the host-wide values. 

```yaml
metavariables:
  cluster:
    env: CLUSTER_NAME               # every source on this host

sources:
  - service: api
    path: /var/log/api/*.log
    metavariables:
      version:
        file: /opt/api/VERSION      # just api
  - service: worker
    path: /var/log/worker.log
    metavariables:
      version:
        file: /opt/worker/VERSION   # just worker
```

## Other features

### System metrics (optional)

weftd can also ship host metrics alongside log lines. Add a top-level
`metrics:` block to turn them on, and delete it to turn them off. An empty
block (`metrics: {}`) runs everything with the defaults:

```
# /etc/weftd/config.yaml
token: wft_a1b2c3d4e5f6...
deployment: web

metrics:
  interval: 30s

sources:
  - service: api
    path: /var/log/api/*.log
```

| Key | Definition | Default |
|---|---|---|
| `interval` | How often to sample, as a duration. A value that doesn't parse falls back to the default. | `30s` |
| `collectors` | Which of `cpu`, `memory`, `disk`, `load`, `network` to run. Unknown names are ignored. | all five |

Samples are read from `/proc` (Linux only) and streamed as JSON log rows.
Metrics describe the host, not any one source, so they go to one place: under
your [`deployment`](#configuration), as if it were a service. A host with
`deployment: web` writes `/log/<namespace>/web/host.log`, `disk.log` and
`network.log`, and every host in the same deployment writes to the same files,
so one query covers the fleet. `deployment` has to be set for metrics to run
(a [`deployment` metavariable](#metavariables) counts, and wins); without one
weftd logs an error and writes nothing. Characters that don't belong in a path
(`/`, `.`, whitespace) become `_`.

| File | One row per | Contents |
|---|---|---|
| `host.log` | tick | `cpu`, `memory` and `load` objects |
| `disk.log` | `/dev/*` mount, per tick | `path`, `total_gb`, `used_gb`, `pct` |
| `network.log` | non-loopback interface, per tick | `iface`, `rx_bytes_sec`, `tx_bytes_sec` |

The objects in `host.log`:

| Object | Fields |
|---|---|
| `cpu` | `user`, `system`, `idle`, `iowait` (percent), `cores` |
| `memory` | `total_mb`, `used_mb`, `available_mb`, `swap_used_mb`, `pct` |
| `load` | `load1`, `load5`, `load15` |

`host.log` holds `cpu`, `memory` and `load` as nested objects, so the first
tick has no `cpu` (it needs two samples to compute a delta) and a collector you
turned off has no object. `network.log` needs two samples too, so it's empty
for the first tick after startup.

The JSON doesn't repeat the time, host or deployment: those are the row's own
columns, which `jq` reads as `$ts`, `$host` and `$deployment`. Because the
shard filters on the column, `select($host == "i-0abc")` narrows a deployment's
shared file to one host before any row is returned.

Query them the same way you query any JSON log source:

```
# Recent memory pressure
tail /log/myns/web/host.log | jq '.memory.pct'

# Per-host, per-mount disk usage, alerting above 80%
tail /log/myns/web/disk.log \
  | jq -r '"\($host):\(.path) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops

# Recent load averages by host
tail -n 1000 /log/myns/web/host.log | jq '{host: $host, load5: .load.load5}'

# One host's disks
tail /log/myns/web/disk.log | jq 'select($host == "i-0abc")'
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

## Behavior and semantics

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

weftd remembers how far it has delivered each tailed file, so a restart picks
up where it left off and reads whatever was written in the meantime. The saved
position means "weft acknowledged everything up to here", so lines
that were in flight when weftd stopped are sent again. 

State is kept in `/var/lib/weftd/checkpoints/`. The `.deb` unit creates it
(`StateDirectory=weftd`). Checkpointing is always on. These settings, all
optional, tune it:

```yaml
checkpoint:
  dir: /var/lib/weftd/checkpoints   # where state.json lives
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
running binary, and exits so systemd (`Restart=always`) relaunches it. Set
`auto_update: false` to pin a host, for example while you investigate a
release. It then only logs a line when a newer version exists.
