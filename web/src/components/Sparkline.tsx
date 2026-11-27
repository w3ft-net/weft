import { useMemo } from 'react'
import {
  LineChart, Line, XAxis, YAxis,
  ReferenceLine, ReferenceArea,
  ResponsiveContainer,
} from 'recharts'
import { type CronResult } from '../lib/api'
import { CHART_COLORS } from './chartColors'
import './Sparkline.css'

export interface SparkPoint {
  ts: number
  value: number
}

export interface SparkSeries {
  tag: string
  color: string
  points: SparkPoint[]
  latest: string
}

// Tagged series never use the untagged cyan, so "(untagged)" stays
// distinguishable when it's drawn alongside tags.
const TAG_PALETTE = [
  '#5fd787', '#af87ff', '#ffaf00', '#5fd7d7',
  '#ff87af', '#87afff', '#d7d75f', '#d787d7',
]

export function colorForTag(tag: string): string {
  if (tag === '') return CHART_COLORS.line
  let h = 0
  for (let i = 0; i < tag.length; i++) {
    h = (h * 31 + tag.charCodeAt(i)) & 0xffff
  }
  return TAG_PALETTE[h % TAG_PALETTE.length]
}

export function groupResultsByTag(results: CronResult[]): SparkSeries[] {
  const byTag = new Map<string, CronResult[]>()
  for (const r of results) {
    const key = r.Tag ?? ''
    if (!byTag.has(key)) byTag.set(key, [])
    byTag.get(key)!.push(r)
  }

  const series: SparkSeries[] = []
  for (const [tag, rows] of byTag) {
    const points: SparkPoint[] = []
    for (const r of rows) {
      const v = r.Value !== null && r.Value !== undefined ? r.Value : parseFloat(r.Stdout?.trim())
      if (typeof v === 'number' && !isNaN(v)) {
        points.push({ ts: new Date(r.RanAt).getTime(), value: v })
      }
    }
    if (points.length === 0) continue
    points.sort((a, b) => a.ts - b.ts)
    series.push({
      tag,
      color: colorForTag(tag),
      points,
      latest: formatSparkValue(points[points.length - 1].value),
    })
  }
  series.sort((a, b) => {
    if (a.tag === '') return -1
    if (b.tag === '') return 1
    return a.tag.localeCompare(b.tag)
  })
  return series
}

export function formatSparkValue(v: number): string {
  if (Number.isInteger(v)) return v.toString()
  return v.toFixed(2).replace(/\.?0+$/, '')
}

// ── Time-axis tick generation + formatting ────────────────────────────────

const TIME_STEPS_MS = [
  60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000,
  3_600_000, 7_200_000, 14_400_000, 43_200_000, 86_400_000,
]

export function niceTimeTicks(tMinMs: number, tMaxMs: number, maxTicks: number): number[] {
  if (maxTicks < 2) return []
  const span = tMaxMs - tMinMs
  if (span <= 0) return []
  let step = TIME_STEPS_MS[TIME_STEPS_MS.length - 1]
  for (const s of TIME_STEPS_MS) {
    if (span / s <= maxTicks) {
      step = s
      break
    }
  }
  const start = Math.ceil(tMinMs / step) * step
  const ticks: number[] = []
  let v = start
  while (v <= tMaxMs) {
    ticks.push(v)
    v += step
    if (ticks.length > maxTicks * 2) break
  }
  return ticks
}

export function formatTimeTick(ms: number, spanMs: number): string {
  const d = new Date(ms)
  if (spanMs > 86_400_000) {
    return `${(d.getUTCMonth() + 1).toString().padStart(2, '0')}/${d.getUTCDate().toString().padStart(2, '0')}`
  }
  return `${d.getUTCHours().toString().padStart(2, '0')}:${d.getUTCMinutes().toString().padStart(2, '0')}`
}

// ── Data merging ──────────────────────────────────────────────────────────

function seriesKey(tag: string): string {
  return tag || '_default'
}

function mergeSeriesData(seriesList: SparkSeries[]): Record<string, number>[] {
  const byTs = new Map<number, Record<string, number>>()
  for (const s of seriesList) {
    const key = seriesKey(s.tag)
    for (const p of s.points) {
      let row = byTs.get(p.ts)
      if (!row) {
        row = { ts: p.ts }
        byTs.set(p.ts, row)
      }
      row[key] = p.value
    }
  }
  return [...byTs.values()].sort((a, b) => a.ts - b.ts)
}

// ── Sparkline component ───────────────────────────────────────────────────

interface SparklineProps {
  series: SparkSeries[]
  threshold?: number | null
  condition?: 'above' | 'below' | 'eq' | 'neq' | null
  height?: number
  showTimeAxis?: boolean
  // xDomain fixes the time axis (ms) to a requested window, widened to any
  // data outside it; absent = first to last point.
  xDomain?: [number, number]
}

const TICK_STYLE = { fontSize: 10, fill: CHART_COLORS.tick, fontFamily: "'JetBrains Mono', monospace" }

export function Sparkline({ series, threshold, condition, height = 48, showTimeAxis = false, xDomain }: SparklineProps) {
  const drawable = series.filter(s => s.points.length >= 2)
  const data = useMemo(() => mergeSeriesData(drawable), [drawable])
  if (data.length < 2) return null

  const hasThresh = typeof threshold === 'number' && !isNaN(threshold)
  const tMin = xDomain ? Math.min(xDomain[0], data[0].ts) : data[0].ts
  const tMax = xDomain ? Math.max(xDomain[1], data[data.length - 1].ts) : data[data.length - 1].ts
  const spanMs = tMax - tMin

  let yMin = Infinity, yMax = -Infinity
  for (const s of drawable) {
    for (const p of s.points) {
      if (p.value < yMin) yMin = p.value
      if (p.value > yMax) yMax = p.value
    }
  }
  if (hasThresh) {
    if ((threshold as number) < yMin) yMin = threshold as number
    if ((threshold as number) > yMax) yMax = threshold as number
  }
  if (yMin === yMax) { yMin -= 0.5; yMax += 0.5 }

  const xTicks = showTimeAxis ? niceTimeTicks(tMin, tMax, 5) : undefined

  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 4 }}>
        <XAxis
          dataKey="ts"
          type="number"
          domain={[tMin, tMax]}
          hide={!showTimeAxis}
          ticks={xTicks}
          tickFormatter={(v: number) => formatTimeTick(v, spanMs)}
          tick={TICK_STYLE}
          axisLine={false}
          tickLine={false}
        />
        <YAxis hide domain={[yMin, yMax]} />

        {hasThresh && (condition === 'above' || condition === 'below') && (
          <ReferenceArea
            y1={condition === 'above' ? yMin : (threshold as number)}
            y2={condition === 'above' ? (threshold as number) : yMax}
            fill={CHART_COLORS.safe}
            ifOverflow="hidden"
          />
        )}

        {drawable.map(s => (
          <Line
            key={s.tag || '_'}
            dataKey={seriesKey(s.tag)}
            stroke={s.color}
            strokeWidth={1}
            dot={false}
            isAnimationActive={false}
            connectNulls={false}
          />
        ))}

        {hasThresh && (
          <ReferenceLine
            y={threshold as number}
            stroke={CHART_COLORS.crit}
            strokeDasharray="4 3"
            data-testid="sparkline-threshold-line"
          />
        )}
      </LineChart>
    </ResponsiveContainer>
  )
}

export function SparkLegend({ series }: { series: SparkSeries[] }) {
  return (
    <ul className="watch-legend" data-testid="watch-legend">
      {series.map(s => (
        <li
          key={s.tag || '_'}
          className="watch-legend-row"
          data-testid={`watch-legend-row-${s.tag || '_'}`}
        >
          <span className="watch-legend-swatch" style={{ color: s.color }}>■</span>
          <span className="watch-legend-tag">{s.tag}</span>
          <span className="watch-legend-value">{s.latest}</span>
        </li>
      ))}
    </ul>
  )
}
