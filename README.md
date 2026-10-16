# Weft

A centralized log analysis service. Connect your AWS accounts, query logs using Unix pipeline idioms (awk/sed/jq/grep), and get results in seconds.

## Quick Start (for customers)

### 1. Register your log source

For each CloudWatch log group you want to connect:

```bash
weft source add myapp/web --log-group /aws/myapp/web --region us-east-1

weft source list
```

This just registers the log-group→service mapping — nothing is ingesting yet.

### 2. Deploy the CloudFormation stack

Weft ingests CloudWatch logs by push, not by assuming a role into your
account: you deploy a small CloudFormation stack in your own AWS account
that forwards a subscription filter through a Kinesis Data Firehose
delivery stream to our HTTPS ingest endpoint. Nothing of ours runs in your
account — Firehose is fully AWS-managed.

```bash
aws cloudformation create-stack \
  --stack-name weft-cloudwatch-firehose \
  --template-url https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml \
  --parameters ParameterKey=WeftToken,ParameterValue=<your-weft-namespace-token> \
               ParameterKey=LogGroupNames,ParameterValue="/aws/myapp/web,/aws/myapp/worker" \
  --capabilities CAPABILITY_IAM
```

Deploy one stack per AWS account/region that owns log groups you want to connect.

### 3. Configure the schema

Once real events start flowing through the stack, weft accumulates a sample
from live traffic — there's nothing to pull on demand, so this happens
naturally as your app logs. Once a sample is ready:

```bash
weft source configure myapp/web
```

This walks you through an interactive schema wizard — it shows you real
lines and asks about the timestamp field, the log level, and any JSON
payloads. The resulting schema is stored server-side; there's no local
schema file to manage. The source starts ingesting as soon as you approve it.

### 4. Query your logs

```bash
# Connect to weft
weft

# List available apps
weft> apps

# Query logs
weft> cat myapp-web.log | head -20
weft> cat myapp-web.log | grep ERROR
weft> cat myapp-web.log | awk '$LEVEL == "ERROR"' | sort | uniq -c
weft> cat myapp-worker.log | grep timeout | wc -l
weft> cat myapp-web.log | jq 'select(.status == 500)' | head -10
weft> cat myapp-web.log | cut -d' ' -f5- | sort | uniq -c
```

## Shell Reference

### Source commands

| Command | Description |
|---------|-------------|
| `cat <app>.log` | All log lines for the app |
| `head [-N] <app>.log` | First N lines (default 10) |
| `tail [-N] <app>.log` | Last N lines (default 10) |
| `cat warp.log` | All lines from all apps |

### Pipeline tools

Standard Unix tools work natively in the shell:

- **awk** — filter and transform: `awk '$LEVEL == "ERROR"'`, `awk '{ print $3 }'`
- **jq** — JSON queries: `jq 'select(.status == 500)'`, `jq '{user: .user_id}'`
- **grep** — text search: `grep timeout`, `grep -i error`, `grep -v healthcheck`
- **sed** — text substitution: `sed 's/foo/bar/'`, `sed -n '/pattern/p'`
- **sort**, **uniq**, **wc**, **cut**, **head**, **tail** — standard data tools

### Meta-variables

In awk expressions, these are injected automatically:

| Variable | Description |
|----------|-------------|
| `$APP` | Application name |
| `$LEVEL` | Log level (INFO, ERROR, etc.) |
| `$TS` | Timestamp |
| `$INSTANCE` | Instance ID |

In jq, use underscore-prefixed names: `._app`, `._level`, `._ts`, `._instance`.

### Predicate pushdown

Weft automatically optimizes queries. Filter stages that can be expressed as SQL are pushed down to the storage layer (DuckDB), so only matching rows are transferred. Non-pushable stages (sort, formatting, aggregation) run in the compute layer.

```
cat myapp.log | awk '$LEVEL == "ERROR"' | sort | uniq -c
                 ↑ pushed to DuckDB        ↑ runs in compute
```

### Built-in commands

| Command | Description |
|---------|-------------|
| `apps` | List available pseudofiles |
| `range` | Show current time range |
| `exit` | Quit the shell |

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| Up/Down | Navigate command history |
| Ctrl+R | Reverse search history |
| Tab | Autocomplete commands and files |
| Shift+Tab | Previous autocomplete match |
| Ctrl+D | Exit |

## Configuration

Create `~/.weft.yaml` or `.weft.yaml` in your project directory:

```yaml
endpoint: weft-nlb-xxxxx.elb.us-east-2.amazonaws.com:9020
customer: mycompany
```

## Log Format Support

Weft detects and parses common log formats:

- **Plain text** — whitespace-delimited fields (`$1`, `$2`, etc.)
- **JSON** — full JSON lines (query with jq)
- **Prefix + JSON** — structured prefix followed by a JSON payload
- **Common Log Format** — Apache/nginx access logs
- **Python logging** — including Celery/Django formats
- **Go log** — standard library log output

The schema detector handles most formats automatically. For unusual formats, edit the generated `.schema.yaml` file directly.

## Architecture

```
weft shell → NLB → Coordinator
                       ├── Shard nodes (DuckDB, time-partitioned)
                       ├── Compute pool (gpipe workers)
                       └── Ingest (weftd agents, CloudWatch push via Firehose, Heroku/syslog drains)
```

- **Coordinator**: routes queries, manages shard discovery, ingest fan-in
- **Shards**: time-partitioned DuckDB instances on EBS
- **Compute**: stateless gpipe workers for sort/uniq/awk-print/jq-transform
- **Ingest**: weftd agents push via gRPC; CloudWatch, Heroku, and syslog sources push via HTTP — see [ingestion-cloudwatch.md](docs/ingestion-cloudwatch.md) and [ingestion-weftd.md](docs/ingestion-weftd.md)
