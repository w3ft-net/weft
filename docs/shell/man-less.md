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

An in-process pager for weft. It runs inside the shell and behaves
the same in the web shell and the native CLI.

When a pipeline ends with `| less` the query runs to completion,
the result is loaded into the pager, and the shell shows
the pager until you press q.

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

## NOT SUPPORTED

- Command-line options. `less` does not read them: `-N`, `-S`, `-R`, `-i`,
  `-F` and the rest are accepted and have no effect.
- Backward search (`?`), case-insensitive or regular-expression search; `/`
  is a case-sensitive substring match.
- Numeric counts (`5j`), half-page scrolling (`d`, `u`), follow mode (`F`),
  opening the text in an editor (`v`) and marks.

## SEE ALSO

`help`, `man alert`, `man range`
