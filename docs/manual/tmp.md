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
set long enough to slice it a few different ways. It behaves the way a
Unix `/tmp` does: write with `>`, append with `>>`, edit with `vi`, list
with `ls`, remove with `rm`.

```
# Save a filtered result set
cat api.log | awk '$LEVEL == "ERROR"' > /tmp/errs

# Take a look
wc -l /tmp/errs
grep timeout /tmp/errs | sort | uniq -c | sort -rn | head

# Keep adding to it later
tail -n 200 api.log | awk '$STATUS >= 500' >> /tmp/errs

# Organise an incident
mkdir /tmp/incident-2026-04-22
cat api.log | grep 'req_id=abc123' > /tmp/incident-2026-04-22/trace
vi /tmp/incident-2026-04-22/notes
ls -l /tmp/incident-2026-04-22

# Compare two result sets
cp /tmp/errs /tmp/errs.bak
tail -n 1000 api.log | awk '$LEVEL == "ERROR"' > /tmp/errs
diff /tmp/errs.bak /tmp/errs           # what's new in the most recent slice
comm /tmp/errs.bak /tmp/errs           # three-column shared/unique view
join orders.csv prices.csv             # relate two result sets by key

# Rename, keep alive
mv /tmp/errs /tmp/incident-errs
touch /tmp/incident-errs               # reset the 48h clock

# Clean up when you're done
rm -rf /tmp/incident-2026-04-22
```

### Files expire after 48 hours

Despite the shell rules, /tmp is durable storage — your files survive
logouts and are visible from every session. But every file is deleted
48 hours after it was last edited. Any of `>`, `>>`, `cp`, `mv`, or
`touch` resets the clock; the mtime shown in `ls -l` is the source of
truth.

`touch /tmp/foo` is the explicit way to extend a file's life without
otherwise changing it — useful when an investigation is running long
and you want to keep your scratch around past the original 48h.

Treat /tmp the way you'd treat a shared whiteboard in an operations
room: great for the investigation you're running right now, not where
you keep anything you'll need next week. For long-term storage of a
pipeline's output, save the pipeline itself as a cron (see the
**Crons** section) — cron results are retained for the full history
your plan covers.

### Storage limits

| Plan    | /tmp space | Per-file limit |
|---------|-----------:|---------------:|
| Trial   |     500 KB |         512 KB |
| Swatch  |      10 MB |         512 KB |
| Yard    |     100 MB |         512 KB |
| Bolt    |       1 GB |         512 KB |

The per-file limit applies on every plan — /tmp is for filtered result
sets and notes, not for parking raw log data. Writes that would exceed
either your total allowance or the per-file cap fail with a clear
error; the partial output is not written.

Check your current usage from the shell:

```
cat /etc/usage
```

### What /tmp is not

- **Not executable.** Files in /tmp are never treated as scripts.
  `vi /tmp/foo` is a text editor opening a text file; typing `foo` at
  the prompt afterwards does nothing. Scripts live in `/bin` — see the
  **Custom scripts** section.
- **Not a source for `tail -f`.** Follow-mode is a streaming query
  against live logs. You can redirect the output of a finished query
  into /tmp, but `tail -f /log/myco/api/main.log > /tmp/stream` is not supported
  in this release.
- **Not shared across accounts.** /tmp is per-customer. Every member
  of your account sees the same /tmp; users from other accounts never
  see any of it.

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
| `wc -l a`          | Lines / words / bytes / characters / longest-line — combines flags freely |

`cp` and `mv` are restricted to /tmp on both sides — copying scratch
text into `/bin` would route around weft's script validation. If
that's actually what you want, `cat /tmp/x > /bin/y` does it through
the normal save path.

### Listing with `ls -l`

`ls` accepts the usual flags — `-l` for long format, `-a` to include
dotfiles, `-h` for human-readable sizes. Long format shows size and
last-modified time, which is how you can tell how much of your 48h
window a file has left:

```
weft> ls -lh /tmp
-rw-r--r--  2.1K  2026-04-24 09:42  errs
drwxr-xr-x     -  2026-04-22 17:03  incident-2026-04-22/
-rw-r--r--   312  2026-04-24 10:15  scratch
```

The same flags work anywhere in the filesystem — `ls -l /bin` shows
when you last edited each of your scripts, `ls -l /cron.d` shows when
each cron was last modified.
