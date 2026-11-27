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
curl -LO https://<your-releases-host>/weftd/v{{WEFTD_VERSION}}/weftd_{{WEFTD_VERSION}}_amd64.deb
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
| `socket` | Path of the records socket for your apps (see [App API](#app-api)). | none; socket disabled |
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
| `kind` | The file part of the pseudofile, `/log/<ns>/<service>/<kind>.log`. `heartbeats`, `runtime`, `host`, `disk` and `network` are reserved (the last three hold [host metrics](#system-metrics-optional)). | `main` |
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
Metrics go under your [`deployment`](#configuration), as if it were a service.
A host with `deployment: web` writes
`/log/<namespace>/web/{host,disk,network}.log`. `deployment` has to be set for
metrics to run; without one weftd logs an error and writes nothing. Depending
on which collectors are enabled, your metrics will end up in:

| File | One row per | Contents |
|---|---|---|
| `host.log` | tick | `cpu`, `memory` and `load` objects |
| `disk.log` | `/dev/*` mount, per tick | `path`, `total_gb`, `used_gb`, `pct` |
| `network.log` | non-loopback interface, per tick | `iface`, `rx_bytes_sec`, `tx_bytes_sec`, `rx_bytes_total`, `tx_bytes_total` |

The objects in `host.log`:

| Object | Fields |
|---|---|
| `cpu` | `user`, `system`, `idle`, `iowait` (percent), `cores`, `time_s` (cumulative busy CPU seconds) |
| `memory` | `total_mb`, `used_mb`, `available_mb`, `swap_used_mb`, `pct` |
| `load` | `load1`, `load5`, `load15` |

`host.log` holds `cpu`, `memory` and `load` as nested objects, so the first
tick has no `cpu` (it needs two samples to compute a delta) and a collector you
turned off has no object. `network.log` needs two samples too, so it's empty
for the first tick after startup.

```
# Recent memory pressure
tail /log/myns/web/host.log | jq '.memory.pct'

# Per-host, per-mount disk usage, alerting above 80%
tail /log/myns/web/disk.log \
  | jq -r '"\($host):\(.path) \(.pct)"' \
  | alert above 80 -v 2 -t 1 --notify=ops

# Recent load averages by host
tail -n 1000 /log/myns/web/host.log | jq '{host: $host, load5: .load.load5}'

# One host's disks
tail /log/myns/web/disk.log | jq 'select($host == "i-0abc")'
```

Metrics only run on Linux (the collectors read `/proc/stat`,
`/proc/meminfo`, `/proc/mounts`, `/proc/loadavg`, and
`/proc/net/dev`). On other kernels the block is ignored.

### Socket listener

weftd can listen on a local Unix socket so your own applications can use the
[weft SDKs](https://github.com/orgs/w3ft-net/repositories) to annotate traces,
spans, and timings and send events, heartbeats and runtime stats over the same
streaming connection weftd already has open.  Name the socket in `config.yaml`
and weftd will create it on startup:

```
# /etc/weftd/config.yaml
socket: /var/run/weftd.sock
```
Some socket properties:

* It's world-writable
* It's one-way -- weft never sends anything back
* It buffers up to 4096 lines, after which, if undrained, writes will silently fail

Though most of the convenience comes from using the weft SDKs, the enterprising
user can always write directly to this socket themselves. Connect to the socket
and write one line per message, each ending in a newline:

```
<app>:<line>\n
```

| Part | Meaning |
|---|---|
| `app` | `service` or `service/kind`, where `kind` defaults to `main`. It must be shorter than 32 characters and contain no spaces. A line with no valid `app:` prefix goes to the default app. |
| `line` | The log line, with no newline in it. A line over 1 MiB closes the connection. |

#### Line header

The weft SDKs write and weftd reads a per-line header carrying metadata for
that line. It writes those headers when it writes to log files and when it
writes to the socket.  

```
\x1e v1 \x1f ts \x1f level \x1f trace \x1f span_name \x1f span_id \x1f parent_id \x1f dur_ns \x1f fmt \x1f kind \x1e body
```

The header sits between two `\x1e` bytes, with its values separated by `\x1f`,
and the spaces above are only for reading. On the socket the header comes right
after `<app>:`.

| Value | Meaning |
|---|---|
| `v1` | The version. It fixes the order and meaning of the values that follow. |
| `ts` | When the line happened, as integer Unix nanoseconds. Empty means weftd stamps it on arrival. |
| `level` | The severity as the writer spells it, such as `INFO`, `WARN` or `ERROR`. |
| `trace` | The trace ID the line belongs to. |
| `span_name` | The name of the span this line describes, for a line that starts or ends a span. |
| `span_id` | The span's ID. |
| `parent_id` | The ID of the span's parent. |
| `dur_ns` | The span's duration in nanoseconds, on the line that ends it. |
| `fmt` | `json` or `text`, the shape of the body. |
| `kind` | A second stream for the same service, such as `queries` for `service/queries`. It uses letters, digits, `-` and `_` (up to 64 characters) and can't be `heartbeats`, `runtime`, `host`, `disk` or `network`. |

Every value after `v1` is optional. A value that doesn't parse, such as a `ts`
that isn't a number or a `dur_ns` that isn't positive, is discarded without
rejecting the line itself. Header field values can't contain `\x1e`, `\x1f` or
a newline. 

## Behavior and semantics

### Delivery

Files are the only source with delivery guarantees. weftd keeps recently sent
rows (up to 8 MiB per file) until the coordinator acknowledges them, at the
`commit_interval` below, and only then advances the saved position. Anything
unacknowledged is resent on a new stream, so delivery is at-least-once.  In the
event of an outage, weftd stops reading when its window is full and resumes
when the upstream is back. Unit (`journalctl`), records-socket and metrics rows
are best-effort: they are sent straight through and are not replayed after a
restart or an outage, whereas log files on disk are checkpointed (see below).

### Resuming after a restart

weftd remembers how far it has delivered each tailed file, so a restart picks
up where it left off and reads whatever was written in the meantime. The saved
position means "weft acknowledged everything up to here", so lines
that were in flight when weftd stopped are sent again. 

State is kept in `/var/lib/weftd/checkpoints/`, which is created by the debian
package (`StateDirectory=weftd`). Checkpointing is always on. These settings,
all optional, tune it:

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
running are read from their first line. Every skip or replay is logged to
weftd's own log (`journalctl -u weftd`), and summarized once a minute as a
`checkpoint stats` line per file.

### Updates

Once a day (or on restart) weftd checks `releases.w3ft.net` for a newer
release. By default it downloads the build, verifies its SHA-256, replaces the
running binary, and exits so systemd (`Restart=always`) relaunches it. Set
`auto_update: false` to pin a host.  It then only logs a line when a newer
version exists.
