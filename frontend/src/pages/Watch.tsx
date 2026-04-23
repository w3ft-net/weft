import { useEffect, useMemo, useState } from 'react'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { useAuth } from '../lib/AuthContext'
import {
  getWatch, getWatchChart,
  type CronWatch, type ChartRange, type ChartPoint, type ChartResponse,
  CHART_RANGES, CHART_RANGE_MS,
} from '../lib/api'
import './Watch.css'

// ── Tag colors ─────────────────────────────────────────────────────────────
// Palette parallels Dashboard.tsx's sparklines so users see the same color
// for the same tag on both views.
const TAG_PALETTE = [
  'var(--accent)', '#7ec8ff', '#a29bfe', '#ffb36b',
  '#6de3a0', '#ff7aa8', '#ffd36b', '#9ef0d8',
]
function colorForTag(tag: string): string {
  if (tag === '') return 'var(--accent)'
  let h = 0
  for (let i = 0; i < tag.length; i++) {
    h = (h * 31 + tag.charCodeAt(i)) & 0xffff
  }
  return TAG_PALETTE[h % TAG_PALETTE.length]
}

// ── Plan retention → which ranges are disabled ─────────────────────────────
// Map the user's tier to its retention cap (in ms) so ranges that would
// exceed the cap can be greyed out. Server is still authoritative; this
// is UX-only.
function retentionMsForTier(tier: string): number {
  switch (tier) {
    case 'swatch': return 30 * 24 * 60 * 60 * 1000
    case 'yard':   return 90 * 24 * 60 * 60 * 1000
    case 'bolt':   return 365 * 24 * 60 * 60 * 1000
    default:       return 24 * 60 * 60 * 1000 // trial
  }
}

// ── Series shaping ─────────────────────────────────────────────────────────

interface Series {
  tag: string
  color: string
  points: ChartPoint[]
  // Window stats for the legend. avg is sample-weighted across buckets.
  count: number
  avg: number
  min: number
  max: number
  latest: number
}

function buildSeries(points: ChartPoint[]): Series[] {
  const byTag = new Map<string, ChartPoint[]>()
  for (const p of points) {
    const k = p.tag ?? ''
    if (!byTag.has(k)) byTag.set(k, [])
    byTag.get(k)!.push(p)
  }

  const series: Series[] = []
  for (const [tag, pts] of byTag) {
    pts.sort((a, b) => a.ts - b.ts)
    let count = 0, sum = 0, min = Infinity, max = -Infinity
    for (const p of pts) {
      count += p.count
      sum += p.value * p.count
      if (p.min < min) min = p.min
      if (p.max > max) max = p.max
    }
    series.push({
      tag,
      color: colorForTag(tag),
      points: pts,
      count,
      avg: count > 0 ? sum / count : 0,
      min: min === Infinity ? 0 : min,
      max: max === -Infinity ? 0 : max,
      latest: pts[pts.length - 1].value,
    })
  }
  // Empty tag first so a single-row watch's unnamed series is always
  // the anchor color.
  series.sort((a, b) => {
    if (a.tag === '') return -1
    if (b.tag === '') return 1
    return a.tag.localeCompare(b.tag)
  })
  return series
}

function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return '—'
  if (Number.isInteger(v)) return v.toString()
  if (Math.abs(v) >= 1000) return v.toFixed(0)
  return v.toFixed(2).replace(/\.?0+$/, '')
}

// ── Chart ──────────────────────────────────────────────────────────────────

interface ChartProps {
  series: Series[]
  hidden: Set<string>
  threshold: number | null
  condition: CronWatch['AlertCondition']
  granularity: ChartResponse['granularity']
  rangeStartMs: number
  rangeEndMs: number
}

function WatchChart({ series, hidden, threshold, condition, granularity, rangeStartMs, rangeEndMs }: ChartProps) {
  const visible = series.filter(s => !hidden.has(s.tag))
  const w = 640
  const h = 240
  const padL = 40 // room for y-tick labels
  const padR = 8
  const padT = 8
  const padB = 22 // room for x-tick labels

  // y-scale rescales to visible tags — the whole point of toggling is
  // to focus. Threshold (if set) extends the range so the line stays
  // in-frame even when no point has crossed it.
  let vMin = Infinity, vMax = -Infinity
  for (const s of visible) {
    for (const p of s.points) {
      if (p.value < vMin) vMin = p.value
      if (p.value > vMax) vMax = p.value
    }
  }
  const hasThresh = typeof threshold === 'number' && !Number.isNaN(threshold)
  if (hasThresh) {
    if ((threshold as number) < vMin) vMin = threshold as number
    if ((threshold as number) > vMax) vMax = threshold as number
  }
  if (!isFinite(vMin) || !isFinite(vMax)) {
    // No visible data — keep the frame usable.
    vMin = 0
    vMax = 1
  }
  if (vMin === vMax) {
    vMin -= 0.5
    vMax += 0.5
  }

  const tMin = rangeStartMs
  const tMax = rangeEndMs
  const tRange = tMax - tMin || 1
  const vRange = vMax - vMin || 1

  const plotW = w - padL - padR
  const plotH = h - padT - padB
  const xFor = (t: number) => padL + ((t - tMin) / tRange) * plotW
  const yFor = (v: number) => padT + plotH - ((v - vMin) / vRange) * plotH

  const thresholdY = hasThresh ? yFor(threshold as number) : null

  const mid = (vMin + vMax) / 2
  const yTicks = [vMin, mid, vMax]

  const startLabel = new Date(tMin).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
  const endLabel = 'now'

  return (
    <div className="watch-chart-wrap">
      <svg
        width={w}
        height={h}
        viewBox={`0 0 ${w} ${h}`}
        className="watch-chart"
        data-testid="watch-chart-svg"
      >
        {/* Safe-side green tint. Same logic as the dashboard sparkline. */}
        {hasThresh && (condition === 'above' || condition === 'below') && thresholdY !== null && (
          <rect
            x={padL}
            y={condition === 'above' ? thresholdY : padT}
            width={plotW}
            height={condition === 'above' ? (padT + plotH) - thresholdY : thresholdY - padT}
            className="watch-chart-safe"
          />
        )}

        {/* y-axis tick lines + labels */}
        {yTicks.map((v, i) => {
          const y = yFor(v)
          return (
            <g key={i} className="watch-chart-ytick">
              <line x1={padL} x2={w - padR} y1={y} y2={y} />
              <text x={padL - 6} y={y + 3} textAnchor="end">{fmtNum(v)}</text>
            </g>
          )
        })}

        {/* x-axis endpoints */}
        <text x={padL} y={h - 6} className="watch-chart-xtick" textAnchor="start">{startLabel}</text>
        <text x={w - padR} y={h - 6} className="watch-chart-xtick" textAnchor="end">{endLabel}</text>

        {/* Min/max band for rollup tiers. Raw data is a single point
            per row so the band collapses to the line; skip the fill
            to avoid muddying. */}
        {granularity !== 'raw' && visible.map(s => {
          if (s.points.length < 2) return null
          const upper = s.points.map(p => `${xFor(p.ts)},${yFor(p.max)}`).join(' ')
          const lower = [...s.points].reverse().map(p => `${xFor(p.ts)},${yFor(p.min)}`).join(' ')
          return (
            <polygon
              key={`band-${s.tag || '_'}`}
              points={`${upper} ${lower}`}
              fill={s.color}
              className="watch-chart-band"
            />
          )
        })}

        {/* Per-tag polyline of avg values */}
        {visible.map(s => {
          if (s.points.length === 0) return null
          const pts = s.points.map(p => `${xFor(p.ts)},${yFor(p.value)}`).join(' ')
          return (
            <polyline
              key={s.tag || '_'}
              points={pts}
              fill="none"
              stroke={s.color}
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          )
        })}

        {/* Threshold line on top */}
        {thresholdY !== null && (
          <line
            x1={padL}
            x2={w - padR}
            y1={thresholdY}
            y2={thresholdY}
            className="watch-chart-threshold"
            data-testid="watch-chart-threshold"
          />
        )}
      </svg>
    </div>
  )
}

// ── Page ───────────────────────────────────────────────────────────────────

export default function Watch() {
  const { name = '' } = useParams()
  const [searchParams, setSearchParams] = useSearchParams()
  const { user } = useAuth()

  const range = (searchParams.get('range') as ChartRange) ?? '24h'
  const hiddenParam = searchParams.get('hide') ?? ''
  const hidden = useMemo(
    () => new Set(hiddenParam ? hiddenParam.split(',').filter(Boolean) : []),
    [hiddenParam],
  )

  const [watch, setWatch] = useState<CronWatch | null>(null)
  const [chart, setChart] = useState<ChartResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  // Watch metadata (name, schedule, pipeline, alert projection). Only
  // reloads when the name param changes.
  useEffect(() => {
    if (!name) return
    getWatch(name)
      .then(setWatch)
      .catch(e => setError(String(e)))
  }, [name])

  // Chart data. Refetches whenever the range qs changes.
  useEffect(() => {
    if (!name) return
    setLoading(true)
    getWatchChart(name, range)
      .then(r => { setChart(r); setError(null) })
      .catch(e => setError(String(e)))
      .finally(() => setLoading(false))
  }, [name, range])

  const series = useMemo(() => buildSeries(chart?.points ?? []), [chart])

  function selectRange(r: ChartRange) {
    const next = new URLSearchParams(searchParams)
    next.set('range', r)
    setSearchParams(next, { replace: true })
  }

  function toggleTag(tag: string) {
    const nextHidden = new Set(hidden)
    if (nextHidden.has(tag)) nextHidden.delete(tag)
    else nextHidden.add(tag)
    const next = new URLSearchParams(searchParams)
    if (nextHidden.size === 0) next.delete('hide')
    else next.set('hide', Array.from(nextHidden).sort().join(','))
    setSearchParams(next, { replace: true })
  }

  const retentionMs = retentionMsForTier(user?.tier ?? 'trial')
  const now = Date.now()
  const rangeStartMs = now - CHART_RANGE_MS[range]

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="watch-detail" data-testid="watch-detail-main">
          <div className="watch-detail-header">
            <Link to="/dashboard" className="watch-detail-back" data-testid="watch-detail-back-link">
              ← Dashboard
            </Link>
            <h1 className="watch-detail-name" data-testid="watch-detail-name">
              {name}
            </h1>
            {watch && (
              <div className="watch-detail-meta">
                <span className="watch-detail-schedule">schedule: {watch.Schedule}</span>
                <span className={`watch-detail-status watch-detail-status-${watch.LastStatus || 'pending'}`}>
                  {watch.LastStatus ? `last: ${watch.LastStatus}` : 'waiting for first run'}
                </span>
              </div>
            )}
            {watch && (
              <pre className="watch-detail-pipeline" data-testid="watch-detail-pipeline">
                {watch.Pipeline}
              </pre>
            )}
          </div>

          <div className="watch-detail-chart-card">
            {error && (
              <div className="watch-detail-error" data-testid="watch-detail-error">{error}</div>
            )}
            {loading && !chart && (
              <div className="watch-detail-loading">loading…</div>
            )}
            {chart && (
              <>
                <WatchChart
                  series={series}
                  hidden={hidden}
                  threshold={watch?.AlertThreshold ?? null}
                  condition={watch?.AlertCondition ?? null}
                  granularity={chart.granularity}
                  rangeStartMs={rangeStartMs}
                  rangeEndMs={now}
                />

                <div className="watch-detail-ranges" data-testid="watch-detail-ranges">
                  {CHART_RANGES.map(r => {
                    const overCap = CHART_RANGE_MS[r] > retentionMs
                    return (
                      <button
                        key={r}
                        type="button"
                        className={`watch-range-btn${r === range ? ' watch-range-btn-active' : ''}${overCap ? ' watch-range-btn-disabled' : ''}`}
                        disabled={overCap}
                        title={overCap ? `beyond ${user?.tier ?? 'trial'} plan retention` : undefined}
                        onClick={() => selectRange(r)}
                        data-testid={`watch-range-${r}-button`}
                      >
                        {r}
                      </button>
                    )
                  })}
                  <span className="watch-detail-gran" data-testid="watch-detail-granularity">
                    granularity: {chart.granularity}
                  </span>
                </div>

                <ul className="watch-detail-legend" data-testid="watch-detail-legend">
                  {series.map(s => {
                    const off = hidden.has(s.tag)
                    const label = s.tag === '' ? '(untagged)' : s.tag
                    return (
                      <li
                        key={s.tag || '_'}
                        className={`watch-detail-legend-row${off ? ' watch-detail-legend-off' : ''}`}
                        data-testid={`watch-detail-legend-row-${s.tag || '_'}`}
                      >
                        <button
                          type="button"
                          className="watch-detail-legend-toggle"
                          onClick={() => toggleTag(s.tag)}
                          data-testid={`watch-detail-legend-toggle-${s.tag || '_'}`}
                        >
                          <span className="watch-detail-legend-swatch" style={{ background: s.color }} />
                          <span className="watch-detail-legend-tag">{label}</span>
                        </button>
                        <span className="watch-detail-legend-stats">
                          n={s.count} · latest {fmtNum(s.latest)} · avg {fmtNum(s.avg)} · min {fmtNum(s.min)} · max {fmtNum(s.max)}
                        </span>
                      </li>
                    )
                  })}
                  {series.length === 0 && (
                    <li className="watch-detail-legend-empty">no data in this range</li>
                  )}
                </ul>
              </>
            )}
          </div>
        </div>
      </main>
    </div>
  )
}
