---
id: manual-tmp
title: Scratch space
web:
  section: tmp
  category: weft-shell
  order: 58
---

## Scratch space (/tmp)

Every weft account has a scratch area at `/tmp`. Use it to save the output
of a pipeline, take notes during an incident, or hold a filtered result
set long enough to slice it a few different ways. Write with `>`, append
with `>>`, edit with `vi`, list with `ls`, remove with `rm`.

```
# Save a filtered result set
cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs

# Take a look
wc -l /tmp/errs
grep timeout /tmp/errs | sort | uniq -c | sort -rn | head

# Keep adding to it later (5xx responses among the last 200 rows)
tail -n 200 /log/myco/api/main.log | awk '$6 ~ /status=5/' >> /tmp/errs

# Organise an incident
mkdir /tmp/incident-2026-04-22
cat /log/myco/api/main.log | grep 'req_id=abc123' > /tmp/incident-2026-04-22/trace
vi /tmp/incident-2026-04-22/notes
ls -l /tmp/incident-2026-04-22

# Compare two result sets
cp /tmp/errs /tmp/errs.bak
tail -n 1000 /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs   # errors among the last 1000 rows
diff /tmp/errs.bak /tmp/errs           # what's new in the most recent slice

# comm and join need sorted input
sort /tmp/errs.bak > /tmp/errs.bak.sorted
sort /tmp/errs > /tmp/errs.sorted
comm /tmp/errs.bak.sorted /tmp/errs.sorted   # three-column shared/unique view

# Relate two saved files by their first column
printf 'api 3\nweb 1\n' > /tmp/errs-by-service
printf 'api oncall-a\nweb oncall-b\n' > /tmp/owners
join /tmp/errs-by-service /tmp/owners        # api 3 oncall-a, web 1 oncall-b

# Rename, keep alive
mv /tmp/errs /tmp/incident-errs
touch /tmp/incident-errs               # reset the 48h clock

# Clean up when you're done
rm -rf /tmp/incident-2026-04-22
```

### Lifetime and limits

- **48 hours.** `/tmp` is durable across logouts and shared by every member of
  your account (no other account sees it), but a file is deleted 48 hours after
  its last write. `>`, `>>`, `cp`, `mv` and `touch` reset the clock; `ls -l`
  shows the mtime. For long-term results, save the pipeline as a cron instead
  (see **Crons**); cron results keep your plan's full history.
- **Size.** A per-file cap and a total allowance, both set by your plan (see
  the **Account** page). A write that would exceed either fails and writes
  nothing. `ls -lh /tmp` shows sizes.
- **Not scripts.** Files in `/tmp` are never executed; scripts live in `/bin`.
- **No follow.** `tail -f ... > /tmp/x` is refused; drop `-f`.
- **Crossing out.** `cp` and `mv` work between `/tmp` and `/bin`, `/cron.d` and the `/etc` config files, and the destination checks the file as it would for `>`: you need the group that manages it, and scripts and crons are validated on save.

The Unix tools for comparing saved files (`diff`, `comm`, `join`, `paste`,
`wc`) are listed in [Builtins](../shell/help-builtins.md#differences-and-limitations).
