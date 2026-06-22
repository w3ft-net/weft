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
ingest directly without installing an agent. You grant a cross-account
IAM role, point weft at a log group, and weft polls CloudWatch on a
schedule (default 30s).

This is the lowest-friction source type: nothing to install, nothing to
run. Latency is bounded by CloudWatch's own ingestion delay plus the
poll interval — usually well under a minute. For sub-second tailing
install weftd instead.

### 1. Create the cross-account IAM role

In the AWS account that owns the log group, create an IAM role that
trusts weft's ingestion account and has read access to the log group.
Pick an external ID — any hard-to-guess string — and keep it secret;
you'll paste it into weft in step 3.

```
# Trust policy — allows weft's ingestion account to assume this role,
# but only when it presents the matching external ID.
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::220664822695:root" },
    "Action": "sts:AssumeRole",
    "Condition": {
      "StringEquals": { "sts:ExternalId": "REPLACE-WITH-YOUR-EXTERNAL-ID" }
    }
  }]
}

# Permissions policy — read-only access to the log group(s) you share.
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "logs:FilterLogEvents",
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams"
    ],
    "Resource": "arn:aws:logs:us-east-2:123456789012:log-group:/aws/lambda/my-fn:*"
  }]
}
```

Scope `Resource` to a single log group (recommended) or a prefix
(`.../log-group:/aws/lambda/*`) to share a whole tier at once. Note the
trailing `:*` — CloudWatch Logs ARNs require it.

### 2. Register the source

Use the `weft` CLI to tell weft about the account and log group. Run this
from any machine with the `weft` binary installed and a valid login.

```
weft source add \
  --namespace myco \
  --service my-app \
  --log-group /aws/lambda/my-fn \
  --account 123456789012 \
  --role-arn arn:aws:iam::123456789012:role/weft-ingest \
  --external-id REPLACE-WITH-YOUR-EXTERNAL-ID \
  --region us-east-2 \
  --interval 30s
```

`source add` pulls a live sample from the log group (the coordinator assumes
the role for you) and runs an interactive schema wizard: it shows real lines
and asks where the timestamp is, which field holds the log level, and whether
the tail is a JSON payload. It detects the common shapes automatically —
CloudWatch-flavored text (`2026-04-18 14:32:01 INFO {"user":"alice"}`) or
pure-JSON Lambda logs — so you usually just confirm its suggestions. The
schema it produces is stored server-side; there's no local file to manage.

Within one poll interval weft will assume the role, page through
recent log events, and start writing rows. Confirm the source is live:

```
weft source list

# And in the shell
cat /log/myco/my-app/main.log | head
```

### Troubleshooting

- **No rows appearing** — the most common causes are a missing
  external ID, the role's resource ARN not matching your log group, or
  the log group being in a different region than you specified. Revisit
  step 1 and confirm each matches.
- **Rows appearing but fields look wrong** — your schema's
  `timestamp_format` is off. Check against Go's reference time
  (`Mon Jan 2 15:04:05 MST 2006`).
- **CloudWatch charges** — weft's poller uses `FilterLogEvents`, which
  AWS bills as data scanned. If cost matters, raise `--interval` (fewer
  calls) or scope your log group tightly.
