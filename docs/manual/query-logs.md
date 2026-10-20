---
id: manual-query-logs
title: Introduction
web:
  section: introduction
  category: weft-shell
  order: 55
---

## The weft shell

The goal of weft is, in a nutshell, to enable efficient querying of distributed
log storage using the time-tested tools of Unix. As such, we've made an effort
to provide dopplegangers of enough of coreutils that you can mostly forget you
aren't just ssh'ed into your servers directly. Stitch together pipelines to
your heart's content. See [Builtins](../shell/help-builtins.md) for a complete list of
supported Unix commands and interpreters.

Some 1-liners are easier for our tools to translate into log queries than
others, and we've invested the most effort into AWK, sed, grep, and jq. Leaning
on these will result in the most efficient retrieval of rows from the log
store. We're working on exposing something like optimization hints in a coming
release.

Again, the principle of least surprise is our north star here. Hopefully you
never know you aren't on an old-school log server. And hopefully when you run
into that surprise, we've got nice clear error messages. But like us, weft is a
work in progress.

## Key differences

### Sandboxness

Weft is a log shell, and although it allows a certain amount of scripting for those purposes, it is not an operating system, a general purpose programming environment, or a VPS you control. There may be things you would like to do that you cannot. The veneer of Unix is only as thick as we needed it to be.

### Range

The timerange of logs you are working with is displayed in your PS1. How far back in time you can set it depends on your plan. Spoiler, if you ship a lot of logs and you wanna query 3 months, it's gonna be slower. You can change it with the `range` command.

### Metavariables

We inject some preset variables into invocations of AWK and jq that are helpful in narrowing down the lines you need; these are all indexed nicely so including them in an AWK pattern or a jq `select()` should get you your log lines faster. See the [full reference](metavars.md) for more details.

## Paths

Depending on what mechanism you are using to ship your logs to weft, the files
where those lines end up are going to look something like this:

```
/log/<namespace>/<service>/<kind>.log          3 segments: all envs
/log/<namespace>/<env>/<service>/<kind>.log    4 segments: one env
```

`<kind>` is the stream: `main` for ordinary logs, plus `heartbeats`,
`metrics` and `runtime` for services that emit telemetry. Which form a
service has depends on whether its producer token was minted with `--env`;
`ls /log/myco` shows env directories next to env-less services.

- `cat /log/myco/prod/api/main.log` reads one env.
- `cat /log/myco/api/main.log` reads that service across every env.
- `cat myco.log` reads the whole namespace; filter with `ENV == "prod"`.
- `ls /log/myco/api` lists a service's kinds.
- Globs only pick kinds inside one service: `/log/myco/prod/api/*.log`.
  Globs that cross services or namespaces (`cat *.log`,
  `/log/myco/*/api/main.log`) are refused.

## Some Examples

```
# Tail logs in real time
tail -f myapp.log

# Errors in the default range (narrow it with `range 1h..`)
cat myapp.log | awk 'LEVEL == "ERROR"'

# Requests by status code
cat nginx.log | jq '.status' | sort | uniq -c | sort -rn

# Everything in a namespace mentioning a failure
cat myco.log | grep "connection refused"

# Most recent 100 lines (cheap)
tail -n 100 myapp.log

# Production errors for one deployment, JSON form
cat /log/myco/prod/api/main.log \
  | jq 'select($level == "ERROR" and $deployment == "api-east")'

# Keep a result set and slice it several ways
cat api.log | awk 'LEVEL == "ERROR"' > /tmp/errs
grep timeout /tmp/errs | sort | uniq -c | sort -rn
```
