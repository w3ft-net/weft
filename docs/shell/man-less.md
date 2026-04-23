---
id: man-less
title: less
shell:
  const: ManLess
  topics:
    - "man less"
    - "less --help"
  banner: "LESS(1)"
---

## NAME

less — page through output one screen at a time

## SYNOPSIS

```
<pipeline> | less
```

## DESCRIPTION

An in-process pager for weft. Unlike the system `less(1)` it runs
inside the shell — no subprocess, no TTY hand-off, and it works
the same way in the web shell as it does in the native CLI.

When a pipeline ends with `| less` the query runs to completion,
the result is loaded into the pager, and the shell enters
ModeLess until you press q.

Help and man pages auto-page when their output would overflow
the viewport — press q to return to the prompt.

## NAVIGATION

```
space, Page Down, f, Ctrl+F   forward one screen
b, Page Up, Ctrl+B            back one screen
j, Down                       forward one line
k, Up                         back one line
g, gg                         jump to top
G                             jump to bottom
q, Ctrl+C                     exit the pager
```

## SEARCH

```
/pattern    search forward (case-sensitive substring)
n           next match
N           previous match
```

## STATUS LINE

Bottom row shows the source title on the left and a percent
position on the right. Incremental search and error messages
replace the status briefly.

## SEE ALSO

`help`, `man alert`, `man range`
