# Ingestion: weftd

## Overview

weftd is a lightweight agent (single Go binary) that runs on customer instances, tails log files directly, and ships structured lines to weft over gRPC. It provides sub-second latency from log write to queryable in the shell — the fast-follow to CloudWatch ingestion.

weftd is optional. CloudWatch ingestion works without any software installed on customer instances. weftd is for customers who want real-time tailing and are willing to deploy an agent.

## Motivation

CloudWatch ingestion has inherent latency: the CloudWatch agent batches logs (typically 5-60 seconds), pushes to CloudWatch, which then delivers via subscription filter to Kinesis. End-to-end latency is seconds to low minutes. For interactive debugging (`tail -f`), this is too slow.

## Architecture

```
Customer instance
  ┌─────────────────────────┐
  │ Application              │
  │   └── writes to /var/log/app.log
  │                          │
  │ weftd                    │
  │   └── tails log files    │
  │   └── applies schema     │
  │   └── gRPC stream ──────────→ Coordinator → Active shard
  └─────────────────────────┘
```

weftd ships to the coordinator (single stable endpoint), not directly to shard nodes. The coordinator routes to the active shard. This means weftd never needs to know about shard topology.

## Protocol

gRPC streaming over TLS, consistent with the internal shard protocol:

```protobuf
service WeftIngest {
  // Long-lived stream: agent pushes log lines as they appear
  rpc Ingest(stream IngestLine) returns (IngestAck);
}

message IngestLine {
  string app = 1;
  string instance = 2;      // hostname or instance ID
  string raw = 3;            // the log line
  int64 ts_unix_ns = 4;      // timestamp from the log or wall clock
  repeated string fields = 5; // pre-split by schema
  string level = 6;          // extracted by schema
  bytes payload_json = 7;    // JSON portion, if applicable
}

message IngestAck {
  int64 lines_received = 1;
}
```

Authentication: the agent includes its token in gRPC metadata on connection. The coordinator maps the token to a customer and validates the app name.

## Onboarding & sampling

A source is not ingested until a human has reviewed a sample and approved a schema for it. The lifecycle is **sampling → pending → configured**:

1. **Sampling.** When weftd starts a source the coordinator doesn't recognize, the agent collects a sample. It does **not** stop at the first line it sees — it accumulates until it has a full sample (`SampleTarget`, currently 50 non-empty lines), reading existing content and then *following* the source (file tail / `journalctl -f`) for the rest. A quiet source blocks here by design rather than submitting a one-line sample that's useless for deriving a schema. While accumulating, the agent ships throttled `partial` progress submits so the source surfaces in the UI/CLI as `sampling` with an `N/target` count — checking it reports `<source> hasn't emitted enough lines to sample yet (12/50)` instead of stalling invisibly.

2. **Pending.** Once the full sample is collected the agent makes one final (non-partial) submit. The coordinator runs schema auto-detection over it and the source becomes `pending` — visible in the onboarding UI and via `weft source pending`, awaiting a human to confirm a schema.

3. **Configured.** A human approves a schema (`weft source configure <ns/app>` or the dashboard). The agent, which has been polling `GetSourceStatus` on a 60s ticker, picks up the schema, compiles it locally, and starts the tailer (see *Agent behavior* below).

### Resampling a configured source

If the originally-derived schema turns out wrong (e.g. the first sample was unrepresentative), a fresh sample can be requested at any time via `weft source resample <ns/app>`, the **Resample** button in the dashboard, or `POST /api/sources/{ns}/{app}/resample`.

Resampling **does not interrupt ingestion**: the source stays `configured` and keeps streaming on its current schema. The agent notices the resample flag on its next status poll and re-runs the sampler *concurrently* with the live tailer (guarded so a quiet source doesn't stack repeated attempts). When the fresh sample completes it becomes available for review; reconfiguring then swaps the schema in place — there is no window where the source stops ingesting.

The current schema for any configured source can be viewed at any time with `weft source schema <ns/app>`, the dashboard's **View schema** disclosure, or `GET /api/sources/{ns}/{app}/schema`.

## Configuration

Local file, designed to be managed by Terraform/Ansible/Chef:

```yaml
# /etc/weftd/config.yaml

endpoint: weft.example.com:443
token: wft_agent_abc123...

sources:
  - service: vimgym
    path: /var/log/vimgym/*.log

  - service: callbox
    path: /var/log/callbox/app.log
```

Parsing is not configured here: weftd ships raw lines and the source's
schema (`text`, `json` or `prefix_json`) lives on the server, set by
detection plus confirmation or by `weft source configure`.

Terraform example:

```hcl
resource "template_file" "weftd_config" {
  template = file("${path.module}/weftd.yaml.tpl")
  vars = {
    endpoint = var.weft_endpoint
    token    = var.weft_agent_token
    app_name = var.app_name
    log_path = var.log_path
  }
}
```

## Agent behavior

### Tailing

weftd uses inotify (Linux) / kqueue (macOS) to watch configured log paths. On new data:

1. Read new lines from the file
2. Apply the configured schema to extract structured fields
3. Buffer briefly (configurable, default 100ms or 100 lines, whichever comes first)
4. Send batch over the gRPC stream

### File rotation

Handles standard log rotation patterns:

- Detects rename/truncate (logrotate copytruncate)
- Follows new file when the watched path is recreated
- Drains the old file to EOF before switching to the new one, and rewinds on truncation
- Persists a delivered-offset checkpoint per file (`/var/lib/weftd/checkpoints/state.json`) so restarts resume without gaps; see `docs/plans/weftd-offset-checkpointing.md` and the manual's "Resuming after a restart"

### Glob patterns

`path: /var/log/vimgym/*.log` watches for new files matching the glob and tails them. Useful for applications that create per-date or per-process log files.

### Backpressure

If the gRPC stream is slow (coordinator overloaded, network issues):

1. Buffer in memory up to a configurable limit (default 10MB)
2. If buffer fills, drop oldest lines and increment a dropped counter
3. On reconnect, log the drop count so the customer knows data was lost
4. No disk buffering in v1 — keep the agent simple and low-footprint

### Reconnection

If the connection to the coordinator drops, weftd retries with exponential backoff (1s, 2s, 4s, ..., max 30s). It continues tailing and buffering locally during disconnection.

## Deployment

Single static Go binary, no dependencies:

```bash
# Install
curl -L https://weft.example.com/dl/weftd-linux-amd64 -o /usr/local/bin/weftd
chmod +x /usr/local/bin/weftd

# Run as systemd service
systemctl enable --now weftd
```

systemd unit file:

```ini
[Unit]
Description=weftd log shipping agent
After=network.target

[Service]
ExecStart=/usr/local/bin/weftd --config /etc/weftd/config.yaml
Restart=always
RestartSec=5
User=weftd
Group=weftd

[Install]
WantedBy=multi-user.target
```

## Open questions

- **Schema sync:** weftd applies the schema locally, but the schema is defined on the coordinator. Should weftd fetch its schema from the coordinator on startup (using the token for auth), or is the local config file the source of truth? Local is simpler for Terraform but risks drift.
- **Disk buffering:** v1 drops data on buffer overflow. Should v2 spill to disk to survive longer outages? Adds complexity and disk I/O but prevents data loss.
- **Multi-line logs:** stack traces, Python tracebacks, etc. span multiple lines. Should weftd support configurable multi-line join patterns (e.g., "lines starting with whitespace are continuations")?
- **Metrics:** should weftd expose a local metrics endpoint (Prometheus `/metrics`) for customers to monitor agent health, throughput, drops?
