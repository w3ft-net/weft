# Plan: chart pipe-stage axis labels (--x-label / --y-label)

## TL;DR

Adding `--x-label` / `--y-label` is **feasible and small** — roughly a half-day of work, all confined to `shellcore/chart/`. The y-label is the only mildly interesting piece; the x-label is a one-liner relative to existing layout machinery. **Confirm v2 deferral** — the work is cheap enough that it's not worth coupling to the pipe-stage delivery, and we'd rather see real demand from `| chart` users before picking the rendering convention (vertical-rotated vs. top-left horizontal).

## 1. What's there today

The chart layout is computed once per `Render()` from three constants and four rects.

**Constants** (`shellcore/chart/axis.go:21-25`):
- `yAxisWidth = 10` — leftmost columns reserved for y-tick labels
- `xAxisHeight = 2` — bottom rows reserved (1 for the rail, 1 for tick labels)
- `titleHeight = 1` — top row reserved for title

**Layout** (`shellcore/chart/axis.go:42-72`, `computeLayout`):
- `l.title` = `{0, 0, totalW, 1}` when `withTitle`
- `l.yAxis` = `{0, top, yw, totalH-top-xh}` — full height minus title and x-axis
- `l.xAxis` = `{left, totalH-xh, totalW-left, xh}` — sits to the right of the y-axis (no overlap)
- `l.plot` = `{left, top, totalW-left, totalH-top-bottomReserve}` — the residual area

There is **no padding between rects** — y-axis abuts plot at column `yAxisWidth-1` (the vertical rail), x-axis rail at row `totalH-xh`. The corner is drawn at `(area.x-1, axisRow)` in `drawXAxis` (`axis.go:221-223`).

**Tick rendering**:
- `drawYAxis` (`axis.go:183-208`) — vertical rail at `area.right()-1`, labels right-aligned in `[area.x, axisCol-1]`, so they live in 9 columns with the rail occupying the 10th.
- `drawXAxis` (`axis.go:213-271`) — horizontal rail at `area.y` (top of the xAxis rect), labels written one row below at `area.y+1`. Tick density is gated by `area.w / 7` for numeric and `area.w` for time, with `minGap` (`axis.go:233-240`) preventing label collision.

**Cell grid** (`shellcore/chart/cell.go:37-79`) is a flat row-major `[]rune` + `[]style`. `drawText` only writes horizontally one rune per column; out-of-bounds writes are silently dropped (`cell.go:62-69`). There is no built-in vertical-text or rotated-glyph path.

**Title** (`builder.go:162-170`) is centered in `l.title` with truncation via `truncateWidth` — exactly the model an x-label would mirror.

## 2. What needs to change for axis labels

### computeLayout

Add two fields and two constants:

```
const (
    xLabelHeight = 1   // reserved row below x-tick labels
    yLabelWidth  = 1   // reserved column left of y-tick labels
)
```

New rects on `layout`:
- `xLabel rect` at `{left, totalH-1, totalW-left, 1}` when `withXLabel`
- `yLabel rect` at `{0, top, 1, totalH-top-xh-xlh}` when `withYLabel`

The existing rect derivations need bumping:
- `bottomReserve += xlh` so `plot.h` shrinks
- `left += ylw` so `plot.x` shifts right and `yAxis.x` shifts to `ylw`
- `title` rect width is unchanged (spans full top regardless)

This is roughly 12-15 lines added to `computeLayout` plus 2 fields on the `layout` struct.

### Y-label vertical text

The cell grid does not handle rotated glyphs. Two options:

**Option A — vertical text (one rune per row)**: walk the y-label string and call `g.set(yLabel.x, yLabel.y+i, r, st)` for each rune. This is the conventional rendering and requires no new infrastructure — `cellGrid.set` already takes (col, row, rune, style). Strings longer than `yLabel.h` get truncated via `truncateWidth` against the height. Total: ~10 lines of new code in a `drawYAxisLabel` helper. **Recommended.**

**Option B — horizontal label at top-left**: write the y-label as horizontal text in the gap between the title row and the y-tick area, e.g. on row `top` columns `[0, yAxisWidth)`. Cheaper visually but unconventional and crowds the topmost y-tick. **Not recommended unless we discover Option A looks bad in practice.**

Note: Unicode width is non-trivial for non-ASCII labels — `drawText` currently advances one column per rune regardless of double-width characters (`cell.go:73-79`). The y-label vertical path inherits that limitation; it's pre-existing tech debt and not made worse by this feature.

### X-label placement

The x-label sits at row `totalH-1`, one row below the x-tick label row. `drawXAxis` writes ticks at `axisRow+1` (`axis.go:267`) which is currently the last row — we need to shrink `xAxis.h` from 2 to stay at 2 (rail + tick labels) while reserving an extra row below for the label. So `bottomReserve = xAxisHeight + xLabelHeight` (= 3) when both are on.

`drawXAxisLabel` is a near-clone of `drawTitle`: center the truncated label inside `l.xLabel`. ~8 lines.

## 3. Builder API additions

```go
func (b *Builder) XAxisLabel(s string) *Builder { b.xLabel = s; return b }
func (b *Builder) YAxisLabel(s string) *Builder { b.yLabel = s; return b }
```

Two new fields on `Builder` (`builder.go:35-45`): `xLabel, yLabel string`. `Render()` (`builder.go:109-157`) gains:

```go
hasXLabel := b.xLabel != "" && withAxis
hasYLabel := b.yLabel != "" && withAxis
l := computeLayout(b.w, b.h, hasTitle, withAxis, hasXLabel, hasYLabel)
if hasXLabel { drawXAxisLabel(g, l.xLabel, b.xLabel) }
if hasYLabel { drawYAxisLabel(g, l.yLabel, b.yLabel) }
```

The `computeLayout` signature gains two bools — internal API, no external callers outside the `chart` package (verified: `grep computeLayout` returns only the file itself plus tests). Backward compat is fine; existing callers pass `false, false` (or we keep the old signature and add a `computeLayoutWithLabels` — slightly noisier but fully additive).

The `NoAxis()` path (`builder.go:94-97`) implicitly disables labels because labels only make sense alongside ticks. No new flag needed.

## 4. Render-cost concerns

Each label eats from the plot:
- y-label: 1 column → plot drops from `w-10` to `w-11` cols
- x-label: 1 row → plot drops from `h-3` to `h-4` rows (with title)

**Pathological cases** at small terminals:
- `chart` minimum useful dims today are roughly `30x10` (tick-label legibility). With both labels: `31x11` for the same plot density.
- At `w=20`: y-axis takes 10, y-label takes 1, leaving 9 columns of plot — still drawable, but `drawXAxis` density (`axis.w/7`) drops from 1 to 1, marginal.
- At `h=6` with title + x-label: plot is `h - 1 - 3 = 2` rows. A line chart in 2 rows is essentially a sparkline.

**Recommendation**: silently drop labels below threshold rather than error. Concretely, in `Render()`:

```go
if hasYLabel && b.w < 20 { hasYLabel = false }
if hasXLabel && b.h < 8  { hasXLabel = false }
```

The thresholds are heuristic — pick them at the border where the *plot itself* still has 2+ usable cells. The `h<8` floor matches the existing single-point/two-point degraded rendering paths (`builder.go:135-138`) which already assume sub-minimal heights are valid inputs.

## 5. Effort estimate

| Area | Lines | Notes |
|---|---|---|
| `axis.go` constants + `computeLayout` | ~20 | Two fields, two constants, signature bump |
| `axis.go` `drawXAxisLabel` / `drawYAxisLabel` | ~25 | Clones of `drawTitle` w/ vertical loop |
| `builder.go` `Builder` fields + setters + `Render` plumb | ~15 | Pure additive |
| `chart_test.go` golden updates + new cases | ~40 | Two new layouts × line/bar |
| **Total** | **~100** | |

**Files touched**: `shellcore/chart/builder.go`, `shellcore/chart/axis.go`, `shellcore/chart/chart_test.go`. No callers outside `chart/` change unless the `| chart` pipe-stage decides to wire the flags through.

**Time**: half-day including tests. Not multi-day. Not 1-hour either — the visual tests need real golden frames.

## 6. Recommendation: defer to v2

**Confirm the user's call.** Reasons:

1. **No real demand yet.** The pipe-stage chart hasn't shipped, so we don't know if users will want labeled axes inline (`| chart --y-label "errors/min"`) or whether they'll just put context in `--title` and call it a day. Watch-driven charts have shipped without axis labels and nobody has complained.
2. **Convention choice is a one-way door for goldens.** Once we pick vertical-rotated y-label and check in golden frames, switching to top-left horizontal later forces a goldens churn. Better to wait until we've seen `| chart` in the wild and have one or two real-world examples to design against.
3. **Cost stays the same later.** This isn't load-bearing on the pipe-stage architecture. The `chart` package is freestanding (`cell.go:1-12` package doc emphasizes renderer independence) — adding labels in v2 is the same half-day whether we do it before or after `| chart` lands.
4. **Pipe-stage flag surface is already wide.** The parent plan wrestles with `--bucket`, `--x`, `--y`. Adding `--x-label`/`--y-label` to that list muddles the v1 conversation. Ship v1 first, see what's actually painful.

**Push back trigger**: if review of the pipe-stage plan reveals a customer who specifically wants axis labels inline, re-cost — but it's a half-day add, not a re-architecture, so v2 timing doesn't compound.
