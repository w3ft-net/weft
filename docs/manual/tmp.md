---
id: manual-tmp
title: Scratch space
web:
  section: tmp
  category: weft-shell
  order: 58
---

## Scratch space (/tmp)

Sometimes a 1-liner just isn't enough. When you need to stage the results of
some grepping and awking, throw those lines in `/tmp`. 

```
cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs
```

Take notes, diff outputs, run comm/join, etc. to your heart's content. Or almost maybe?

### Lifetime and limits

- **48 hours.** We know your homedir hygiene is terrible, so `/tmp` files are
  deleted 48 hours after its last write. `>`, `>>`, `cp`, `mv` and `touch`
  reset the clock; `ls -l` shows the mtime. If you want to accumulate longer-term
  results, use [crons](crons.md) instead.
- **Size.** A per-file cap and a total allowance, both set by your plan (see
  the **Account** page). A write that would exceed either fails. `ls -lh /tmp` shows sizes.
- **No scripts.** Files in `/tmp` are not executable. If you want to run it, put it in `/bin`.
