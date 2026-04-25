---
id: manual-weftd
title: Install weftd
web:
  section: install-weftd
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
rows to an app named `<namespace>/metrics`. If you set `deployment`,
the app becomes `<namespace>/<deployment>_metrics` so that different
tiers of hosts land in separate pseudofiles.

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

# 5-minute load average by host
cat /log/myns/metrics.log --since 1h \
  | jq 'select(.type == "load") | {host, load5}'
```

Metrics only run on Linux (the collectors read `/proc/stat`,
`/proc/meminfo`, `/proc/mounts`, `/proc/loadavg`, and
`/proc/net/dev`). On other kernels the block is ignored.
