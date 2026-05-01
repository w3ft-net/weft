import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  listDashboards, getDashboard,
  type DashboardListItem, type DashboardResponse,
  type DashboardTile, type DashboardWatchPayload,
} from '../lib/api'
import './Dashboards.css'

export default function Dashboards() {
  const { name } = useParams<{ name?: string }>()
  const [list, setList] = useState<DashboardListItem[] | null>(null)
  const [listError, setListError] = useState<string | null>(null)

  useEffect(() => {
    listDashboards()
      .then(setList)
      .catch(e => setListError(e.message ?? String(e)))
  }, [])

  return (
    <div className="dashboards-page">
      <SidebarNav />
      <div className="dashboards-body">
        <DashboardSidebar list={list} listError={listError} selectedName={name} />
        <main className="dashboards-main">
          {name
            ? <DashboardView name={name} />
            : <DashboardEmptyState hasAny={list !== null && list.length > 0} />}
        </main>
      </div>
    </div>
  )
}

// ── Sidebar of named dashboards ────────────────────────────────────────────

function DashboardSidebar({
  list, listError, selectedName,
}: {
  list: DashboardListItem[] | null
  listError: string | null
  selectedName?: string
}) {
  return (
    <nav className="dashboards-sidebar" data-testid="dashboards-sidebar">
      <h2 className="dashboards-sidebar-heading">Dashboards</h2>
      {listError && <p className="dashboards-sidebar-error">{listError}</p>}
      {list === null && !listError && <p className="dashboards-sidebar-loading">Loading…</p>}
      {list && list.length === 0 && (
        <p className="dashboards-sidebar-empty">No dashboards yet.</p>
      )}
      {list && list.length > 0 && (
        <ul className="dashboards-sidebar-list">
          {list.map(d => (
            <li key={d.name}>
              <Link
                to={`/dashboards/${encodeURIComponent(d.name)}`}
                className={`dashboards-sidebar-item${d.name === selectedName ? ' dashboards-sidebar-item-active' : ''}`}
                data-testid={`dashboards-sidebar-link-${d.name}`}
              >
                <span className="dashboards-sidebar-name">{d.name}</span>
                {d.title && d.title !== d.name && (
                  <span className="dashboards-sidebar-title">{d.title}</span>
                )}
                {d.invalid && <span className="dashboards-sidebar-invalid">invalid</span>}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </nav>
  )
}

// ── Empty / index view ─────────────────────────────────────────────────────

function DashboardEmptyState({ hasAny }: { hasAny: boolean }) {
  return (
    <div className="dashboards-empty" data-testid="dashboards-empty-state">
      <h1>{hasAny ? 'Pick a dashboard' : 'No dashboards yet'}</h1>
      <p>
        Dashboards are YAML files under <code>/etc/dashboards/</code>. Create one
        from the shell:
      </p>
      <pre className="dashboards-empty-snippet">vi /etc/dashboards/prod.conf</pre>
      <p>
        Each dashboard groups watch-driven tiles into sections. See the{' '}
        <Link to="/manual" data-testid="dashboards-empty-manual-link">manual</Link>{' '}
        for the YAML grammar and tile recipes.
      </p>
    </div>
  )
}

// ── Detail view ────────────────────────────────────────────────────────────

function DashboardView({ name }: { name: string }) {
  const [data, setData] = useState<DashboardResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setData(null)
    setError(null)
    getDashboard(name)
      .then(setData)
      .catch(e => setError(e.message ?? String(e)))
  }, [name])

  if (error) {
    return <div className="dashboards-error" data-testid="dashboards-error">{error}</div>
  }
  if (!data) {
    return <div className="dashboards-loading">Loading…</div>
  }

  return (
    <div className="dashboards-view" data-testid={`dashboards-view-${name}`}>
      {data.spec.title && <h1 className="dashboards-title">{data.spec.title}</h1>}
      {data.spec.sections.map((section, si) => (
        <section
          key={si}
          className="dashboards-section"
          data-testid={`dashboards-section-${si}`}
        >
          {section.title && (
            <h2 className="dashboards-section-title">{section.title}</h2>
          )}
          <div className="dashboards-grid">
            {section.tiles.map((tile, ti) => (
              <Tile
                key={ti}
                tile={tile}
                payload={data.watches[tile.watch] ?? null}
              />
            ))}
          </div>
        </section>
      ))}
    </div>
  )
}

// ── Tile dispatch ──────────────────────────────────────────────────────────

function Tile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload | null
}) {
  const size = tile.size ?? 'md'
  const className = `dashboards-tile dashboards-tile-${size}`
  return (
    <div
      className={className}
      data-testid={`dashboards-tile-${tile.watch}`}
    >
      <Link
        to={`/watch/${encodeURIComponent(tile.watch)}`}
        className="dashboards-tile-header"
      >
        <span className="dashboards-tile-watch">{tile.watch}</span>
        <span className="dashboards-tile-kind">{tile.kind}</span>
      </Link>
      <div className="dashboards-tile-body">
        {payload === null
          ? <NoData />
          : <TileBody tile={tile} payload={payload} />}
      </div>
    </div>
  )
}

function NoData() {
  return <div className="dashboards-tile-nodata">no data</div>
}

function TileBody({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  switch (tile.kind) {
    case 'counter': return <CounterTile tile={tile} payload={payload} />
    case 'gauge':   return <GaugeTile tile={tile} payload={payload} />
    case 'sparkline': return <SparklineTile tile={tile} payload={payload} />
    case 'table':   return <TableTile tile={tile} payload={payload} />
    default:        return <div>unknown kind: {String(tile.kind)}</div>
  }
}

// ── Counter ────────────────────────────────────────────────────────────────

function CounterTile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  const latest = latestValue(payload)
  const tone = thresholdTone(tile.threshold, latest)
  const text = latest === null
    ? '—'
    : tile.format
      ? formatString(tile.format, latest)
      : formatNum(latest)
  return (
    <div className={`dashboards-counter dashboards-tone-${tone}`} data-testid={`counter-${tile.watch}`}>
      {text}
    </div>
  )
}

// ── Gauge ──────────────────────────────────────────────────────────────────

function GaugeTile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  const latest = latestValue(payload)
  const max = tile.threshold?.max ?? 100
  const tone = thresholdTone(tile.threshold, latest)
  const pct = latest === null || max <= 0 ? 0 : Math.max(0, Math.min(1, latest / max))
  return (
    <div className="dashboards-gauge" data-testid={`gauge-${tile.watch}`}>
      <div className="dashboards-gauge-bar">
        <div
          className={`dashboards-gauge-fill dashboards-tone-${tone}`}
          style={{ width: `${pct * 100}%` }}
        />
      </div>
      <div className="dashboards-gauge-value">
        {latest === null ? '—' : formatNum(latest)} / {formatNum(max)}
      </div>
    </div>
  )
}

// ── Sparkline ──────────────────────────────────────────────────────────────

function SparklineTile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  const points = useMemo(() => {
    return payload.results
      .filter(r => r.Value !== null)
      .map(r => ({ t: new Date(r.RanAt).getTime(), v: r.Value as number }))
      .sort((a, b) => a.t - b.t)
  }, [payload])

  if (points.length < 2) {
    return <NoData />
  }

  const w = 200
  const h = 48
  const pad = 2

  let tMin = points[0].t, tMax = points[points.length - 1].t
  let vMin = Infinity, vMax = -Infinity
  for (const p of points) {
    if (p.v < vMin) vMin = p.v
    if (p.v > vMax) vMax = p.v
  }
  const tRange = tMax - tMin || 1
  const vRange = vMax - vMin || 1

  const polyline = points.map(p => {
    const x = pad + ((p.t - tMin) / tRange) * (w - pad * 2)
    const y = h - pad - ((p.v - vMin) / vRange) * (h - pad * 2)
    return `${x},${y}`
  }).join(' ')

  const last = points[points.length - 1]
  const lastX = pad + ((last.t - tMin) / tRange) * (w - pad * 2)
  const lastY = h - pad - ((last.v - vMin) / vRange) * (h - pad * 2)

  const tone = thresholdTone(tile.threshold, last.v)

  return (
    <div className="dashboards-sparkline-wrap" data-testid={`sparkline-${tile.watch}`}>
      <svg width="100%" height={h} viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="dashboards-sparkline">
        <polyline
          points={polyline}
          fill="none"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <circle cx={lastX} cy={lastY} r="2.5" />
      </svg>
      <div className={`dashboards-sparkline-latest dashboards-tone-${tone}`}>
        {formatNum(last.v)}
      </div>
    </div>
  )
}

// ── Table ──────────────────────────────────────────────────────────────────

function TableTile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  const cols = tile.columns ?? []
  const rows = useMemo(() => {
    if (payload.results.length === 0) return []
    const stdout = payload.results[0].Stdout?.trim() ?? ''
    if (!stdout) return []
    return stdout.split('\n').slice(0, 8).map(line => line.trim().split(/\s+/))
  }, [payload])

  if (rows.length === 0) return <NoData />

  return (
    <table className="dashboards-table" data-testid={`table-${tile.watch}`}>
      <thead>
        <tr>{cols.map(c => <th key={c}>{c}</th>)}</tr>
      </thead>
      <tbody>
        {rows.map((row, i) => (
          <tr key={i}>
            {cols.map((_, ci) => <td key={ci}>{row[ci] ?? ''}</td>)}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ── Helpers ────────────────────────────────────────────────────────────────

function latestValue(payload: DashboardWatchPayload): number | null {
  for (const r of payload.results) {
    if (r.Value !== null) return r.Value
  }
  return null
}

function thresholdTone(
  th: DashboardTile['threshold'],
  v: number | null,
): 'normal' | 'warn' | 'crit' {
  if (v === null || !th) return 'normal'
  if (typeof th.crit === 'number' && v >= th.crit) return 'crit'
  if (typeof th.warn === 'number' && v >= th.warn) return 'warn'
  return 'normal'
}

function formatNum(v: number): string {
  if (!Number.isFinite(v)) return '—'
  if (Number.isInteger(v)) return v.toString()
  return v.toFixed(2)
}

// formatString applies a printf-like %d / %f / %s to a single number value.
// Keeps the implementation tiny — just enough for tile.format strings.
function formatString(fmt: string, v: number): string {
  return fmt.replace(/%(\.\d+)?[dfis]/g, m => {
    if (m.includes('d') || m.includes('i')) return Math.round(v).toString()
    if (m.includes('f')) {
      const match = m.match(/\.(\d+)/)
      const digits = match ? Number(match[1]) : 2
      return v.toFixed(digits)
    }
    return v.toString()
  })
}
