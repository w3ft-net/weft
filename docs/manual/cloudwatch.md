---
id: manual-cloudwatch
title: Connect a CloudWatch source
web:
  section: connect-cloudwatch
  category: connect-source
  order: 40
---

## Connect a CloudWatch source

If your logs already land in AWS CloudWatch Logs — Lambda, ECS/Fargate,
API Gateway, RDS, or anything else that writes to a log group — weft can
ingest them with no agent. You deploy a small CloudFormation
stack in your own AWS account that forwards a CloudWatch Logs subscription
filter through a Kinesis Data Firehose delivery stream to weft.
Firehose pushes events to weft as they arrive.

This source type has nothing to install and nothing to run. Latency is
CloudWatch's subscription-filter delivery plus Firehose's buffering (roughly
a minute at the lowest buffering setting the CloudFormation template uses).
For sub-second tailing, install weftd.

### 1. Register the source

Use the `weft` CLI to tell weft which log group maps to which service
(the `--service` you pick becomes the `<service>` path segment; see
**Namespaces**).
Run this from any machine with the `weft` binary installed and a valid
login.

```
weft source add \
  --namespace myco \
  --service my-app \
  --log-group /aws/lambda/my-fn \
  --region us-east-2
```

This records the mapping. Ingestion starts after the later steps. The source
shows up as pending in `weft source list` (or run `weft source pending`):

```
weft source list
```

### 2. Deploy the CloudFormation stack

In the AWS account that owns the log group, deploy the Firehose stack.
It provisions the Firehose delivery stream, the IAM roles CloudWatch Logs
needs to write to it, the subscription filter on your log group(s), and a
required S3 bucket for any events Firehose can't deliver (30-day expiry).

```
aws cloudformation create-stack \
  --stack-name weft-cloudwatch-firehose \
  --template-url https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml \
  --parameters ParameterKey=WeftToken,ParameterValue=<your-weft-namespace-token> \
               ParameterKey=LogGroupNames,ParameterValue="/aws/lambda/my-fn" \
  --capabilities CAPABILITY_IAM
```

`WeftToken` is a namespace token (`weft token create --namespace myco`,
for a namespace that already exists). It authenticates Firehose's deliveries
to weft the same way any other push source authenticates. `LogGroupNames` is
a comma-separated list, so one stack can subscribe several log groups in the
same account/region.

To add or remove log groups, update the stack's `LogGroupNames` parameter and
redeploy. The Firehose stream and IAM roles stay unchanged.

### 3. Configure the schema

Once your log group starts emitting events, weft accumulates a sample
from that traffic as your app logs. Once enough lines have accumulated:

```
weft source configure myco/my-app
```

This runs an interactive schema wizard: it shows real lines and asks
where the timestamp is, which field holds the log level, and whether the
tail is a JSON payload. It detects the common shapes automatically —
CloudWatch-flavored text (`2026-04-18 14:32:01 INFO {"user":"alice"}`) or
JSON Lambda logs — so you usually confirm its suggestions. The
schema it produces is stored server-side. The source starts ingesting as
soon as you approve it.

```
# And in the shell
cat /log/myco/my-app/main.log | head
```

That path reads the service across every env. If the token you deployed
carries an env, the rows live under `/log/myco/<env>/my-app/main.log`; see
[Paths](query-logs.md#paths).

### Troubleshooting

- **No rows appearing** — check the Firehose delivery stream's own
  CloudWatch log group (created by the stack, separate from your app's
  log group) for delivery errors; a common cause is the `WeftToken`
  parameter not matching a live namespace token. Also confirm the
  subscription filter attached — CloudWatch allows at most 2
  subscription filters per log group, so if you already forward this log
  group elsewhere (Datadog, Splunk) you may be out of slots.
- **Nothing to configure yet** — `weft source configure` needs a sample.
  A low-traffic log group can take a while to accumulate one.
  `weft source schema <namespace>/<service>` shows accumulation progress.
- **Rows appearing but fields look wrong** — your schema's
  `timestamp_format` is off. Check against Go's reference time
  (`Mon Jan 2 15:04:05 MST 2006`), then `weft source resample <namespace>/<service>` to
  collect a fresh sample, then re-run the wizard once it completes.
