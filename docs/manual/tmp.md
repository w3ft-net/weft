---
id: manual-tmp
title: Saving results to /tmp
web:
  section: tmp
  category: query
  order: 57
---

## Saving results to /tmp

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

### Files expire after 48 hours

/tmp is durable storage — your files survive logouts and are visible
from every session. Every file is deleted
48 hours after it was last edited. Any of `>`, `>>`, `cp`, `mv`, or
`touch` resets the clock; the mtime shown in `ls -l` is the source of
truth.

`touch /tmp/foo` extends a file's life and leaves its content
unchanged. Use it when an investigation runs long and you want to keep
your scratch past the original 48h.

/tmp holds the data for the investigation you are running now. For
long-term storage of a pipeline's output, save the pipeline itself as a
cron (see **Crons**). Cron results are retained for the full history
your plan covers.

### Storage limits

/tmp has a total allowance and a per-file cap, both set by your plan;
see the Account page for your plan's limits. Use /tmp for filtered result
sets and notes. Writes that would exceed
either limit fail with an error; the partial output is not written.

Check what you're using from the shell. `ls -lh` shows each file's
size:

```
ls -lh /tmp
```

`cat /etc/usage` reports your log-ingest quota.

### Limits

- **Scripts.** Files in /tmp are never treated as scripts.
  `vi /tmp/foo` opens a text file; typing `foo` at the prompt
  afterwards does nothing. Scripts live in `/bin` — see
  **Custom scripts**.
- **Follow mode.** Redirect the output of a finished query into /tmp.
  `tail -f /log/myco/api/main.log > /tmp/stream` is refused with an
  error, because a follow never finishes and the file would hold only
  a truncated read. Drop `-f` to save a finished query.
- **Sharing.** /tmp is per-customer. Every member of your account
  sees the same /tmp; users from other accounts see none of it.

### Working on saved results

Once you have a couple of saved result sets, the standard Unix
analysis kit is available for comparing and combining them:

| Command            | What it does                                                              |
|--------------------|--------------------------------------------------------------------------|
| `cp a b`           | Duplicate a file                                                          |
| `mv a b`           | Rename a file                                                             |
| `touch a`          | Reset the 48h clock without otherwise changing the file                   |
| `diff a b`         | Show line-level differences between two files                             |
| `comm a b`         | Three-column merge of two sorted files: only-in-A, only-in-B, in-both    |
| `join a b`         | Relational join on a key column (default: whitespace, column 1 each side) |
| `paste a b`        | Column-merge two files line-by-line (default delimiter: tab)              |
| `wc a`             | Lines, words and bytes; `-l -w -c -m -L` pick columns and combine freely  |

`cp` and `mv` work only inside /tmp, on both sides. /bin, /cron.d and
/etc each have their own save path (permissions and validation), so
copying or moving into or out of them is refused. To put a file there,
use `cat /tmp/x > /bin/y`; to copy one out for editing, use
`cat /bin/y > /tmp/x`.

### Listing with `ls -l`

`ls` accepts the usual flags — `-l` for long format, `-a` to include
dotfiles, `-h` for human-readable sizes. Long format shows size and
last-modified time, which is how you can tell how much of your 48h
window a file has left:

```
weft> ls -lh /tmp
-rw-r--r--  Alice Chen  2.1K  2026-04-24 09:42  errs
drwxr-xr-x  Alice Chen     -  2026-04-22 17:03  incident-2026-04-22/
-rw-r--r--  Alice Chen   312  2026-04-24 10:15  scratch
```

The same flags work anywhere in the filesystem — `ls -l /bin` shows
when you last edited each of your scripts, `ls -l /cron.d` shows when
each cron was last modified.
