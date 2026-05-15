# Plan: chart pipe-stage axis labels (--x-label / --y-label)

## TL;DR

Adding `--x-label` / `--y-label` is **feasible and small**, but it is
a **two-repo change**, not a single in-repo edit. The chart rendering
engine lives in the **warp sibling repo** (`warp/pkg/shellcore/chart/`,
imported here as `github.com/kevlarcon/warp/pkg/shellcore/chart`); the
weft repo only has a thin pipe-stage wrapper (`chart/parse.go`,
`chart/render.go`). So the work splits into a warp PR (the layout +
draw + golden work) and a weft PR (parse the flags, thread them
through). Still roughly a half-day of engineering, but it now carries
warp's release/`replace`-bump coordination cost on top. **Confirm v2
deferral** — cheap enough that it shouldn't be coupled to the
pipe-stage delivery, and we'd rather see real demand from `| chart`
users before picking the rendering convention (vertical-rotated vs.
top-left horizontal) and freezing goldens against it.

> Note: warp is a shared module with its own consumers, so a change
> to `pkg/shellcore/chart` is not weft-local. Keep the Builder API
> additions purely additive (new setters, defaulted layout args) so
> existing warp consumers and weft's wrapper keep compiling, then bump
> the `../warp` replace / tag.

## 1. What's there today

All line/coordinates below are in the **warp** repo
(`/Users/paul/dev/warp/pkg/shellcore/chart/`), verified against the
current checkout. The chart layout is computed once per `Render()`
from three constants and four rects.

**Constants** (`warp .../chart/axis.go:21-25`):
- `yAxisWidth = 10` — leftmost columns reserved for y-tick labels
- `xAxisHeight = 2` — bottom rows reserved (1 for the rail, 1 for tick labels)
- `titleHeight = 1` — top row reserved for title

**Layout** (`warp .../chart/axis.go:42`, `computeLayout(totalW,
totalH int, withTitle, withAxis bool) layout`; `layout` struct at
`axis.go:34`):
- `l.title` = `{0, 0, totalW, 1}` when `withTitle`
- `l.yAxis` = `{0, top, yw, totalH-top-xh}` — full height minus title and x-axis
- `l.xAxis` = `{left, totalH-xh, totalW-left, xh}` — sits to the right of the y-axis (no overlap)
- `l.plot` = `{left, top, totalW-left, totalH-top-bottomReserve}` — the residual area

There is **no padding between rects** — y-axis abuts plot at column `yAxisWidth-1` (the vertical rail), x-axis rail at row `totalH-xh`. The corner is drawn in `drawXAxis` (`warp .../chart/axis.go:213`).

**Tick rendering**:
- `drawYAxis` (`warp .../chart/axis.go:183`) — vertical rail at `area.right()-1`, labels right-aligned in `[area.x, axisCol-1]`, so they live in 9 columns with the rail occupying the 10th.
- `drawXAxis` (`warp .../chart/axis.go:213`) — horizontal rail at `area.y` (top of the xAxis rect), labels written one row below. Tick density is gated by `area.w / 7` for numeric and `area.w` for time, with a `minGap` preventing label collision.

**Cell grid** (`warp .../chart/cell.go`) is a flat row-major `[]rune` + `[]style`; `cellGrid.set(col, row, rune, style)` is the primitive. There is no built-in vertical-text or rotated-glyph path; out-of-bounds writes are silently dropped.

**Title** (`drawTitle`, `warp .../chart/builder.go:162`) is centered in `l.title` with truncation — exactly the model an x-label would mirror.

## 2. What needs to change for axis labels (warp side)

### computeLayout

Add two constants and two layout rects:

```
const (
    xLabelHeight = 1   // reserved row below x-tick labels
    yLabelWidth  = 1   // reserved column left of y-tick labels
)
```

New rects on `layout`:
- `xLabel rect` at `{left, totalH-1, totalW-left, 1}` when `withXLabel`
- `yLabel rect` at `{0, top, 1, totalH-top-xh-xlh}` when `withYLabel`

Existing rect derivations bump: `bottomReserve += xlh` (plot.h
shrinks), `left += ylw` (plot.x shifts right, yAxis.x shifts to
`ylw`); `title` width unchanged. ~12-15 lines in `computeLayout` plus
2 fields on `layout`. **Keep `computeLayout` backward-compatible** —
either default the two new bools or add a `computeLayoutWithLabels`
variant — because `Render()` is the only caller but the Builder it
backs *does* have an external consumer (weft's `chart/render.go`).

### Y-label vertical text

The cell grid does not handle rotated glyphs. Two options:

**Option A — vertical text (one rune per row)**: walk the y-label
string and call `g.set(yLabel.x, yLabel.y+i, r, st)` per rune. No new
infrastructure — `cellGrid.set` already takes (col, row, rune,
style). Truncate against `yLabel.h`. ~10 lines in a
`drawYAxisLabel` helper. **Recommended.**

**Option B — horizontal label at top-left**: write the y-label
horizontally in the gap between the title row and the y-tick area.
Cheaper visually but unconventional and crowds the topmost y-tick.
**Not recommended unless Option A looks bad in practice.**

Unicode width is non-trivial for non-ASCII labels — the cell grid
advances one column per rune regardless of double-width characters.
The vertical y-label path inherits that pre-existing limitation; not
made worse by this feature.

### X-label placement

The x-label sits at row `totalH-1`, one row below the x-tick label
row, so `bottomReserve = xAxisHeight + xLabelHeight` (= 3) when both
are on. `drawXAxisLabel` is a near-clone of `drawTitle`: center the
truncated label inside `l.xLabel`. ~8 lines.

## 3. Builder API additions (warp side)

```go
func (b *Builder) XAxisLabel(s string) *Builder { b.xLabel = s; return b }
func (b *Builder) YAxisLabel(s string) *Builder { b.yLabel = s; return b }
```

Two new fields on `Builder` (`warp .../chart/builder.go:35`):
`xLabel, yLabel string`. `Render()` (`builder.go:109`) gains:

```go
hasXLabel := b.xLabel != "" && withAxis
hasYLabel := b.yLabel != "" && withAxis
l := computeLayout(b.w, b.h, hasTitle, withAxis /*, hasXLabel, hasYLabel*/)
if hasXLabel { drawXAxisLabel(g, l.xLabel, b.xLabel) }
if hasYLabel { drawYAxisLabel(g, l.yLabel, b.yLabel) }
```

These setters are **purely additive** and chain like the existing
`Title`/`Points`/`XAxisTime`/`BarValues`/`NoAxis` setters
(`builder.go:71/76/81/94/101`), so warp's other consumers and weft's
wrapper are unaffected until they opt in. The `NoAxis()` path
(`builder.go:94`) implicitly disables labels because labels only
make sense alongside ticks — no new flag needed.

## 4. Weft side (this repo) — wiring the flags through

This is the second, smaller PR, and it can only land after the warp
change is tagged and the `../warp` replace is bumped.

1. **`chart/parse.go`** — add `XLabel, YLabel string` to `PipeArgs`
   (struct at `parse.go:80`), and two flag cases in `ParsePipeArgs`
   mirroring the existing `case a == "--title":` block
   (`parse.go:156-160`): `--x-label` / `--y-label`, each requiring a
   value, else `chart: --x-label requires a value`.
2. **`chart/render.go`** — in `RenderFromStream`, after the existing
   `if args.Title != "" { b = b.Title(args.Title) }` for both the
   `bar` (line ~47) and default `line` (line ~62) branches, add
   `if args.XLabel != "" { b = b.XAxisLabel(args.XLabel) }` and the
   y-label equivalent. (Sparkline has no axes — skip it, same as
   Title is skipped there today.)
3. **Tests** — extend `chart/pipe_test.go` with a `--x-label` /
   `--y-label` case asserting the labels reach the rendered frame.

## 5. Render-cost concerns

Each label eats from the plot: y-label 1 column, x-label 1 row.

**Pathological cases** at small terminals:
- `chart` minimum useful dims are roughly `30x10`. With both labels: `31x11` for the same plot density.
- At `w=20`: y-axis 10 + y-label 1 → 9 plot columns; still drawable, `axis.w/7` density marginal.
- At `h=6` with title + x-label: plot is `h - 1 - 3 = 2` rows — essentially a sparkline.

**Recommendation**: silently drop labels below threshold rather than
error, in warp's `Render()`:

```go
if hasYLabel && b.w < 20 { hasYLabel = false }
if hasXLabel && b.h < 8  { hasXLabel = false }
```

Thresholds heuristic — pick them where the plot still has 2+ usable
cells. The `h<8` floor matches warp's existing degraded
single/two-point rendering paths.

## 6. Effort estimate

| Repo | Area | Lines | Notes |
|---|---|---|---|
| warp | `chart/axis.go` constants + `computeLayout` | ~20 | Two fields, two constants, defaulted/variant signature |
| warp | `chart/axis.go` `drawXAxisLabel` / `drawYAxisLabel` | ~25 | Clones of `drawTitle` w/ vertical loop |
| warp | `chart/builder.go` fields + setters + `Render` plumb | ~15 | Pure additive |
| warp | `chart/chart_test.go` golden updates + new cases | ~40 | Two new layouts × line/bar |
| weft | `chart/parse.go` + `chart/render.go` + `chart/pipe_test.go` | ~25 | Flag parse + thread-through + test |
| | **Total** | **~125** | plus a warp tag + `../warp` replace bump |

**Files touched**: warp — `pkg/shellcore/chart/{builder,axis,chart_test}.go`; weft — `chart/{parse,render,pipe_test}.go`. **Time**: half-day of engineering, plus the warp release round-trip.

## 7. Recommendation: defer to v2

**Confirm the user's call.** Reasons:

1. **No real demand yet.** Watch-driven and pipe-stage charts have shipped without axis labels and nobody has asked. We don't yet know if users want labeled axes inline (`| chart --y-label "errors/min"`) or will just use `--title`.
2. **Convention choice is a one-way door for goldens.** Once we pick vertical-rotated y-label and check in warp golden frames, switching to top-left horizontal later forces a goldens churn — in a *shared* repo. Wait until we've seen `| chart` in the wild.
3. **Cost stays the same later.** Not load-bearing on the pipe-stage architecture; the chart engine is freestanding. Same half-day whether before or after.
4. **Cross-repo coordination is real overhead.** Even at ~125 lines, this is a warp PR + tag + replace-bump + weft PR. Not worth interleaving with the pipe-stage v1 flag-surface debate (`--bucket`/`--x`/`--y`). Ship v1, see what's actually painful.

**Push-back trigger**: if a customer specifically wants axis labels inline, re-cost — but it's a half-day add, not a re-architecture, so v2 timing doesn't compound.
