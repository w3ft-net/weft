---
id: manual-cloudwatch
title: CloudWatch
web:
  section: connect-cloudwatch
  category: connect-source
  order: 40
---

## Connect a CloudWatch source

If your logs already land in AWS CloudWatch Logs (Lambda, ECS/Fargate, API
Gateway, RDS, etc.), you can deploy a small CloudFormation stack in your own
AWS account that forwards a CloudWatch Logs subscription filter through a
Kinesis Data Firehose delivery stream to weft.

### Create a token

Create a namespaced token as described in [Connect a log
source](namespaces.md#creating-a-token). There's nothing special about Cloudwatch
that requires its own token, but it's probably tidier if it's got its own.

### Register the source

Use the `weft` CLI to tell weft which log group maps to which service. For
example, if we wanted to add the service `lovelace` to the `loom` namespace:

```
weft source add loom/lovelace \
  --log-group /aws/lambda/lovelace \
  --region us-east-2
```

The source will now show up as pending in `weft source list` or `weft source
pending` until you get logs flowing.

### Deploy the CloudFormation stack

In the AWS account and region that own the log group, deploy the Firehose
stack. Create one stack per account and region you want to connect using the
following 1-liner:

```
aws cloudformation create-stack \
  --stack-name weft-cloudwatch-firehose \
  --template-url https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml \
  --parameters ParameterKey=WeftToken,ParameterValue=<your-weft-namespace-token> \
               ParameterKey=LogGroupNames,ParameterValue="/aws/lambda/lovelace" \
  --capabilities CAPABILITY_IAM
```

`LogGroupNames` is a comma-separated list, so one stack can subscribe several
log groups in the same account/region.

Running the above will create the following resources:

| Resource | Purpose |
|---|---|
| Kinesis Data Firehose delivery stream | Receives your log events from CloudWatch Logs and POSTs them over HTTPS to weft's ingest endpoint, authenticated with your token. It buffers up to 1 MB or 60 seconds, whichever comes first, and retries a failed delivery for up to 5 minutes. |
| S3 bucket | Holds only the records weft's endpoint couldn't accept, gzipped under `failed/` and `errors/`, and deletes them after 30 days (Firehose requires one for this kind of destination) |
| CloudWatch log group `/aws/kinesisfirehose/weft-<stack name>` | The delivery stream's own delivery-attempt logs, kept for 14 days |
| IAM role for Firehose | Lets Firehose write that log group and the S3 bucket |
| IAM role for CloudWatch Logs | Lets CloudWatch Logs put records on this stream, assumable only by log groups in this account and region |
| Lambda function and its IAM role | Small function that runs when the stack is created, updated or deleted. It maintains subscription filters named `weft` for all currently log groups. |
| Per-log-group subscription filter | Forwards every event in that group to the delivery stream (created by the Lambda above, don't appear as stack resources) |

To add or remove log groups, update the stack's `LogGroupNames` parameter and
redeploy the stack.

### Approve a schema

Once your log group starts emitting events, weft accumulates a sample from that
traffic and the source goes from `sampling` to `pending`. If you want nice,
speedy lookups, [approve a
schema](connect-source.md#sampling-logs-for-a-schema).

### Finding your logs

Weft maps each registered CloudWatch log group to one service, and every event
in it lands in that service's `main.log`:

| CloudWatch log group | Registered as | weft file |
|---|---|---|
| `/aws/lambda/lovelace` | `loom/lovelace` | `/log/loom/lovelace/main.log` |
| `/ecs/myapp/web` | `loom/web` | `/log/loom/web/main.log` |

Each row carries the event's own CloudWatch timestamp, so rows sort by when the
event happened, not when it arrived. Rows have no `instance`, because the
CloudWatch log stream name isn't kept. Events from a log group you haven't
registered with `weft source add` are dropped.

(The presence of the `<env>` segment depends on how you created your token.
See [Paths](query-logs.md#paths) for the layout rules.)
