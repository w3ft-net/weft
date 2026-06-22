# Weft

A centralized log analysis service. Connect your AWS accounts, query logs using Unix pipeline idioms (awk/sed/jq/grep), and get results in seconds.

## Quick Start (for customers)

### 1. Create an IAM role in your AWS account

Weft needs read-only access to your CloudWatch Logs. Create a role in each AWS account that has log groups you want to query:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::220664822695:root"
      },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": {
          "sts:ExternalId": "<your-external-id>"
        }
      }
    }
  ]
}
```

Attach this policy to the role:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "logs:FilterLogEvents",
        "logs:DescribeLogGroups",
        "logs:DescribeLogStreams",
        "logs:GetLogEvents"
      ],
      "Resource": "arn:aws:logs:*:<your-account-id>:*"
    }
  ]
}
```

Or use the provided CloudFormation template:

```bash
aws cloudformation create-stack \
  --stack-name weft-log-reader \
  --template-url https://weft-public.s3.amazonaws.com/cfn/weft-log-reader.yaml \
  --parameters ParameterKey=ExternalId,ParameterValue=<your-external-id> \
  --capabilities CAPABILITY_NAMED_IAM
```

### 2. Provide us with

- **Role ARN**: `arn:aws:iam::<your-account>:role/WeftLogReader`
- **External ID**: the value you chose above
- **Log group mapping**: which CloudWatch log groups map to which app names

Example:

| Log Group | App Name | Region |
|-----------|----------|--------|
| `/aws/myapp/web` | myapp-web | us-east-1 |
| `/aws/myapp/worker` | myapp-worker | us-east-1 |
| `/ecs/api-prod` | api | us-west-2 |

### 3. Register your log sources

Once we've configured your account on our side, register each log group with
`weft source add`. Weft pulls a live sample from the log group (assuming the
role above) and walks you through an interactive schema wizard — it shows you
real lines and asks about the timestamp field, the log level, and any JSON
payloads. The resulting schema is stored server-side; there's no local schema
file to manage.

```bash
weft source add \
  --namespace myapp \
  --service web \
  --log-group /aws/myapp/web \
  --account <your-aws-account-id> \
  --role-arn arn:aws:iam::<your-account>:role/WeftLogReader \
  --external-id <your-external-id> \
  --region us-east-1

weft source list
```

Weft begins polling your CloudWatch logs immediately (default: every 30 seconds).

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

Standard Unix tools work because weft uses the [warp](https://github.com/graybeardcorp/warp) interpreter:

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
                       ├── Compute pool (warp/gpipe workers)
                       └── CloudWatch poller (cross-account)
```

- **Coordinator**: routes queries, manages shard discovery, polls CloudWatch
- **Shards**: time-partitioned DuckDB instances on EBS
- **Compute**: stateless gpipe workers for sort/uniq/awk-print/jq-transform
- **Poller**: assumes cross-account IAM roles, calls FilterLogEvents, ingests into shards
