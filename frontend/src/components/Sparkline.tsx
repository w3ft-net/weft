import { type CronResult } from '../lib/api'
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

const TAG_PALETTE = [
  'var(--accent)',
  '#7ec8ff',
  '#a29bfe',
  '#ffb36b',
  '#6de3a0',
  '#ff7aa8',
  '#ffd36b',
  '#9ef0d8',
]

function colorForTag(tag: string): string {
  if (tag === '') return 'var(--accent)'
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

interface SparklineProps {
  series: SparkSeries[]
  threshold?: number | null
  condition?: 'above' | 'below' | 'eq' | 'neq' | null
  viewWidth?: number
  viewHeight?: number
}

export function Sparkline({ series, threshold, condition, viewWidth = 120, viewHeight = 30 }: SparklineProps) {
  const drawable = series.filter(s => s.points.length >= 2)
  if (drawable.length === 0) return null

  const w = viewWidth
  const h = viewHeight
  const pad = 2

  let tMin = Infinity, tMax = -Infinity, vMin = Infinity, vMax = -Infinity
  for (const s of drawable) {
    for (const p of s.points) {
      if (p.ts < tMin) tMin = p.ts
      if (p.ts > tMax) tMax = p.ts
      if (p.value < vMin) vMin = p.value
      if (p.value > vMax) vMax = p.value
    }
  }
  const hasThresh = typeof threshold === 'number' && !Number.isNaN(threshold)
  if (hasThresh) {
    if ((threshold as number) < vMin) vMin = threshold as number
    if ((threshold as number) > vMax) vMax = threshold as number
  }
  const tRange = tMax - tMin || 1
  const vRange = vMax - vMin || 1

  const yForValue = (v: number) =>
    h - pad - ((v - vMin) / vRange) * (h - pad * 2)

  const thresholdY = hasThresh ? yForValue(threshold as number) : null

  return (
    <svg width="100%" height={h} viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="watch-sparkline">
      {hasThresh && (condition === 'above' || condition === 'below') && thresholdY !== null && (
        <rect
          x={0}
          y={condition === 'above' ? thresholdY : 0}
          width={w}
          height={condition === 'above' ? h - thresholdY : thresholdY}
          className="watch-sparkline-safe"
        />
      )}

      {drawable.map(s => {
        const pts = s.points.map(p => {
          const x = pad + ((p.ts - tMin) / tRange) * (w - pad * 2)
          const y = yForValue(p.value)
          return `${x},${y}`
        }).join(' ')
        const last = s.points[s.points.length - 1]
        const lastX = pad + ((last.ts - tMin) / tRange) * (w - pad * 2)
        const lastY = yForValue(last.value)
        return (
          <g key={s.tag || '_'}>
            <polyline
              points={pts}
              fill="none"
              stroke={s.color}
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <circle cx={lastX} cy={lastY} r="2.5" fill={s.color} />
          </g>
        )
      })}

      {thresholdY !== null && (
        <line
          x1={0}
          x2={w}
          y1={thresholdY}
          y2={thresholdY}
          className="watch-sparkline-threshold"
          data-testid="sparkline-threshold-line"
        />
      )}
    </svg>
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
          <span className="watch-legend-swatch" style={{ background: s.color }} />
          <span className="watch-legend-tag">{s.tag}</span>
          <span className="watch-legend-value">{s.latest}</span>
        </li>
      ))}
    </ul>
  )
}
