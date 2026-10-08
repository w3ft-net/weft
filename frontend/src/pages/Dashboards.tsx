import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { Sparkline, SparkLegend, groupResultsByTag, type SparkBand } from '../components/Sparkline'
import {
  listDashboards, getDashboard,
  type DashboardListItem, type DashboardResponse,
  type DashboardTile, type DashboardWatchPayload,
} from '../lib/api'
import './Dashboards.css'

const LAST_DASHBOARD_KEY = 'weft_last_dashboard'

export default function Dashboards() {
  const { name } = useParams<{ name?: string }>()
  const navigate = useNavigate()
  const [list, setList] = useState<DashboardListItem[] | null>(null)
  const [listError, setListError] = useState<string | null>(null)

  useEffect(() => {
    listDashboards()
      .then(setList)
      .catch(e => setListError(e.message ?? String(e)))
  }, [])

  useEffect(() => {
    if (name || list === null) return
    if (list.length === 0) return
    if (list.length === 1) {
      navigate(`/dashboards/${encodeURIComponent(list[0].name)}`, { replace: true })
      return
    }
    const last = localStorage.getItem(LAST_DASHBOARD_KEY)
    const target = (last && list.some(d => d.name === last))
      ? last
      : list[0].name
    navigate(`/dashboards/${encodeURIComponent(target)}`, { replace: true })
  }, [name, list, navigate])

  useEffect(() => {
    if (name) {
      localStorage.setItem(LAST_DASHBOARD_KEY, name)
    }
  }, [name])

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="dashboards-content">
          {listError && (
            <div className="dashboards-error" data-testid="dashboards-error">{listError}</div>
          )}
          {name
            ? <DashboardView name={name} list={list} />
            : list !== null && list.length === 0 && <DashboardEmptyState />}
        </div>
      </main>
    </div>
  )
}

// ── Switcher tab strip ─────────────────────────────────────────────────────

// DashboardSwitcher is a one-line tab strip, like a tmux window list:
//   prod │ billing │ edge
// with the current dashboard in inverse video. The list lives under
// data-testid="dashboards-switcher-select" (historical name from when it
// was a <select>); each tab is dashboards-switcher-tab-<name>.
function DashboardSwitcher({ list, currentName }: {
  list: DashboardListItem[]
  currentName: string
}) {
  if (list.length <= 1) return null
  return (
    <nav className="dashboards-switcher" aria-label="dashboards" data-testid="dashboards-switcher-select">
      {list.map(d => (
        <Link
          key={d.name}
          to={`/dashboards/${encodeURIComponent(d.name)}`}
          replace
          title={d.title && d.title !== d.name ? d.title : undefined}
          aria-current={d.name === currentName ? 'page' : undefined}
          className={`dashboards-switcher-tab${d.name === currentName ? ' dashboards-switcher-tab-active' : ''}`}
          data-testid={`dashboards-switcher-tab-${d.name}`}
        >
          {d.name}
        </Link>
      ))}
    </nav>
  )
}

// ── Empty state ────────────────────────────────────────────────────────────

function DashboardEmptyState() {
  return (
    <div className="dashboards-empty" data-testid="dashboards-empty-state">
      <h1 className="tui-cursor">no dashboards yet</h1>
      <p>
        Dashboards are YAML files under <code>/etc/dashboards/</code>. Create one
        from the shell:
      </p>
      <pre className="dashboards-empty-snippet">vi /etc/dashboards/prod.yaml</pre>
      <p>
        Each dashboard groups cron-driven tiles into sections. See the{' '}
        <Link to="/manual#dashboards" data-testid="dashboards-empty-manual-link">manual</Link>{' '}
        for the YAML grammar and tile recipes.
      </p>
    </div>
  )
}

// ── Detail view ────────────────────────────────────────────────────────────

function DashboardView({ name, list }: { name: string; list: DashboardListItem[] | null }) {
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
    return <div className="dashboards-loading">loading {name}…</div>
  }

  return (
    <div className="dashboards-view" data-testid={`dashboards-view-${name}`}>
      {list && <DashboardSwitcher list={list} currentName={name} />}
      <header className="dashboards-head">
        <h1 className="dashboards-title">{data.spec.title || name}</h1>
        {data.updated_at && (
          <span className="dashboards-updated">updated {relTime(data.updated_at)}</span>
        )}
      </header>
      {data.spec.sections.map((section, si) => (
        <CollapsibleSection
          key={si}
          title={section.title}
          testId={`dashboards-section-${si}`}
        >
          <div className="dashboards-grid">
            {section.tiles.map((tile, ti) => (
              <Tile
                key={ti}
                tile={tile}
                payload={data.watches[tile.watch] ?? null}
              />
            ))}
          </div>
        </CollapsibleSection>
      ))}
    </div>
  )
}

// ── Collapsible section ───────────────────────────────────────────────────

function CollapsibleSection({ title, testId, children }: {
  title?: string
  testId: string
  children: React.ReactNode
}) {
  const [collapsed, setCollapsed] = useState(false)
  return (
    <section className="dashboards-section" data-testid={testId}>
      {title && (
        <h2 className="dashboards-section-title">
          <button
            type="button"
            className="dashboards-section-toggle"
            onClick={() => setCollapsed(c => !c)}
            aria-expanded={!collapsed}
            data-testid={`section-toggle-${title}`}
          >
            <span className="dashboards-section-chevron">{collapsed ? '▸' : '▾'}</span>
            {title.toLowerCase()}
          </button>
          <span className="dashboards-section-rule" aria-hidden="true" />
        </h2>
      )}
      {!collapsed && children}
    </section>
  )
}

// ── Tile dispatch ──────────────────────────────────────────────────────────

function Tile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload | null
}) {
  const size = tile.size ?? 'md'
  const className = `tui-pane dashboards-tile dashboards-tile-${size}`
  const lastRun = payload?.watch.LastRunAt
  const failed = !!payload?.watch.LastStatus && payload.watch.LastStatus !== 'ok'
  return (
    <div
      className={className}
      data-testid={`dashboards-tile-${tile.watch}`}
    >
      <div className="tui-pane-head">
        <Link
          to={`/watch/${encodeURIComponent(tile.watch)}`}
          className="tui-pane-title dashboards-tile-header"
          data-testid={`dashboards-tile-${tile.watch}-link`}
        >
          <span className="dashboards-tile-watch">{tile.watch}</span>
        </Link>
        {payload && (
          <span className={`tui-pane-actions dashboards-tile-meta${failed ? ' dashboards-tone-crit' : ''}`}>
            {failed ? payload.watch.LastStatus : lastRun ? relTime(lastRun) : 'pending'}
          </span>
        )}
      </div>
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
  // A format like "%d lines/5m" renders as a big number with the rest of
  // the string as a dim unit. A format with no placeholder is treated as
  // a bare unit suffix.
  const { num, before, after } = splitFormat(tile.format, latest)
  return (
    <div className={`dashboards-counter dashboards-tone-${tone}`} data-testid={`counter-${tile.watch}`}>
      {before && <span className="dashboards-counter-unit">{before}</span>}
      <span className="dashboards-counter-num">{num}</span>
      {after && <span className="dashboards-counter-unit">{after}</span>}
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
      <div className="dashboards-gauge-bar" role="meter" aria-valuenow={latest ?? undefined} aria-valuemin={0} aria-valuemax={max}>
        <span className="dashboards-gauge-bracket">[</span>
        <span className="dashboards-gauge-track">
          <span
            className={`dashboards-gauge-fill dashboards-gauge-${tone}`}
            style={{ width: `${pct * 100}%` }}
          />
        </span>
        <span className="dashboards-gauge-bracket">]</span>
      </div>
      <div className="dashboards-gauge-value">
        <span className={`dashboards-gauge-num dashboards-gauge-${tone}`}>
          {latest === null ? '—' : formatNum(latest)}
        </span>
        <span className="dashboards-counter-unit">/{formatNum(max)}</span>
        <span className="dashboards-gauge-pct">{Math.round(pct * 100)}%</span>
      </div>
    </div>
  )
}

// ── Sparkline ──────────────────────────────────────────────────────────────

function SparklineTile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload
}) {
  const series = useMemo(
    () => groupResultsByTag(payload.results.slice(0, 50)),
    [payload],
  )

  if (series.length === 0 || series.every(s => s.points.length < 2)) {
    return <NoData />
  }

  const bands: SparkBand[] = []
  if (typeof tile.threshold?.warn === 'number') bands.push({ y: tile.threshold.warn, tone: 'warn' })
  if (typeof tile.threshold?.crit === 'number') bands.push({ y: tile.threshold.crit, tone: 'crit' })

  const tagged = series.some(s => s.tag !== '')
  const latest = !tagged && series.length === 1 ? series[0].latest : null
  const tone = latest !== null ? thresholdTone(tile.threshold, parseFloat(latest)) : 'normal'

  return (
    <div className="dashboards-sparkline-wrap" data-testid={`sparkline-${tile.watch}`}>
      {!tagged && latest !== null && (
        <div className={`dashboards-sparkline-latest dashboards-tone-${tone}`}>
          {latest}
        </div>
      )}
      <Sparkline
        series={series}
        bands={bands}
        threshold={payload.watch.AlertThreshold}
        condition={payload.watch.AlertCondition}
        height={80}
        showTimeAxis
      />
      {tagged && <SparkLegend series={series} />}
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
    <table className="tui-table dashboards-table" data-testid={`table-${tile.watch}`}>
      <thead>
        <tr>{cols.map((c, ci) => <th key={c} className={isNumericCol(rows, ci) ? 'dashboards-num' : undefined}>{c}</th>)}</tr>
      </thead>
      <tbody>
        {rows.map((row, i) => (
          <tr key={i}>
            {cols.map((_, ci) => (
              <td key={ci} className={isNumericCol(rows, ci) ? 'dashboards-num' : undefined}>{row[ci] ?? ''}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ── Helpers ────────────────────────────────────────────────────────────────

function isNumericCol(rows: string[][], ci: number): boolean {
  return rows.length > 0 && rows.every(r => r[ci] === undefined || /^-?[\d.,]+%?$/.test(r[ci]))
}

function splitFormat(fmt: string | undefined, v: number | null): { num: string; before: string; after: string } {
  if (v === null) return { num: '—', before: '', after: '' }
  if (!fmt) return { num: formatNum(v), before: '', after: '' }
  const m = fmt.match(/%(\.\d+)?[dfis]/)
  if (!m || m.index === undefined) return { num: formatNum(v), before: '', after: ` ${fmt.trim()}` }
  return {
    num: formatString(m[0], v),
    before: fmt.slice(0, m.index),
    after: fmt.slice(m.index + m[0].length),
  }
}

// relTime renders a timestamp as a short "3m ago" the way top/k9s show age.
function relTime(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(ms)) return ''
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s ago`
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.round(m / 60)
  if (h < 48) return `${h}h ago`
  return `${Math.round(h / 24)}d ago`
}

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
