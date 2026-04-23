---
id: manual-query-logs
title: Query your logs
web:
  section: query-logs
  order: 55
---

## Query your logs

Open the **Shell** or use the `weft` CLI. Log sources appear as
pseudofiles — use standard Unix pipeline idioms:

```
# Tail logs in real time
tail -f myapp.log

# Search for errors in the last hour
cat myapp.log | awk '$LEVEL == "ERROR"'

# Count requests by status code
cat nginx.log | jq '.status' | sort | uniq -c | sort -rn

# Grep across all sources
cat *.log | grep "connection refused"

# Time-bounded queries
cat myapp.log --since 2h --until 30m | head -100
```

The weft query planner analyzes your full pipeline and pushes
predicates down into SQL — you write familiar shell commands, weft
makes them fast.

See `help builtins` in the shell for the full command list, and
`man range` for time-window syntax.
