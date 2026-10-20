---
id: manual-cookbook
title: Cookbook
web:
  section: cookbook
  category: weft-shell
  order: 59
---

## Cookbook

Worked one-liners for common jobs. Several of them save results to
[scratch space](tmp.md) so you can compare or revisit them later.

### Triage a spike

```
cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs
awk '{ print SERVICE, HOST }' /tmp/errs | sort | uniq -c | sort -rn | head
```

### Level histogram by region for one deployment

```
cat /log/myco/prod/api/main.log \
  | awk 'DEPLOYMENT == "api" {print REGION, LEVEL}' \
  | sort | uniq -c
```

### Prod errors for one deployment, across the namespace

```
cat myco.log \
  | awk 'ENV == "prod" && LEVEL == "ERROR" && DEPLOYMENT == "api"' \
  | head -50
```

`ENV` and `DEPLOYMENT` narrow the read before any row is parsed. The jq form
is `jq 'select($env == "prod" and $deployment == "api")'`.

### Whole stack traces for matching errors

```
cat /log/myco/api/main.log | awk 'LINE_NO == 0 && /timeout/' > /tmp/first-lines
```

Lines of one multi-line event share `EVENT_ID`; `LINE_NO == 0` is the first.

### What's new since an hour ago

```
cp /tmp/errs /tmp/errs.bak
tail -n 1000 /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs
diff /tmp/errs.bak /tmp/errs
```

For `comm`, sort first: `sort /tmp/a > /tmp/a.s`.

### Relate two saved files

```
printf 'api 3\nweb 1\n' > /tmp/errs-by-service
printf 'api oncall-a\nweb oncall-b\n' > /tmp/owners
join /tmp/errs-by-service /tmp/owners     # api 3 oncall-a / web 1 oncall-b
```

### Organise an incident

```
mkdir /tmp/incident-2026-04-22
cat /log/myco/api/main.log | grep 'req_id=abc123' > /tmp/incident-2026-04-22/trace
vi /tmp/incident-2026-04-22/notes
touch /tmp/incident-2026-04-22/trace      # keep it past 48h
rm -rf /tmp/incident-2026-04-22           # when done
```
