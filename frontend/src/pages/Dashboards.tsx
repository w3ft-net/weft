import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { Sparkline, SparkLegend } from '../components/Sparkline'
import { PlotBars } from '../components/ShellChartModal'
import { plotToSpark, plotXDomain } from '../lib/chartPlot'
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
                payload={data.crons[tile.cron] ?? null}
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

export function Tile({ tile, payload }: {
  tile: DashboardTile
  payload: DashboardWatchPayload | null
}) {
  const size = tile.size ?? 'md'
  const className = `tui-pane dashboards-tile dashboards-tile-${size}`
  const lastRun = payload?.cron.LastRunAt
  const failed = !!payload?.cron.LastStatus && payload.cron.LastStatus !== 'ok'
  return (
    <div
      className={className}
      data-testid={`dashboards-tile-${tile.cron}`}
    >
      <div className="tui-pane-head">
        <Link
          to={`/cron/${encodeURIComponent(tile.cron)}`}
          className="tui-pane-title dashboards-tile-header"
          data-testid={`dashboards-tile-${tile.cron}-link`}
        >
          <span className="dashboards-tile-watch">{tile.title ?? tile.cron}</span>
        </Link>
        {payload && (
          <span className={`tui-pane-actions dashboards-tile-meta${failed ? ' dashboards-tone-crit' : ''}`}>
            {failed ? payload.cron.LastStatus : lastRun ? relTime(lastRun) : 'pending'}
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
    case 'counter': return <CounterTile tile={tile} />
    case 'gauge':   return <GaugeTile tile={tile} />
    case 'sparkline':
    case 'line':    return <SparklineTile tile={tile} payload={payload} />
    case 'bar':     return <BarTile tile={tile} />
    case 'table':   return <TableTile tile={tile} />
    default:        return <div>unknown kind: {String(tile.kind)}</div>
  }
}

// ── Counter ────────────────────────────────────────────────────────────────

function CounterTile({ tile }: { tile: DashboardTile }) {
  if (tile.error) return <TileError tile={tile} />
  const tone = tile.critical ? 'crit' : 'normal'
  // The server formats the text (tile.display); a format like "%d lines/5m"
  // arrives as num "42" and after " lines/5m", shown as a dim unit.
  const { num, before = '', after = '' } = tile.display ?? { num: '—' }
  return (
    <div className={`dashboards-counter dashboards-tone-${tone}`} data-testid={`counter-${tile.cron}`}>
      {before && <span className="dashboards-counter-unit">{before}</span>}
      <span className="dashboards-counter-num">{num}</span>
      {after && <span className="dashboards-counter-unit">{after}</span>}
    </div>
  )
}

// ── Gauge ──────────────────────────────────────────────────────────────────

function GaugeTile({ tile }: { tile: DashboardTile }) {
  if (tile.error) return <TileError tile={tile} />
  // The first number shown: on a tagged cron, its highest, since points come
  // in table order.
  const latest = tile.points?.find(p => p.value !== undefined && p.value !== null)?.value ?? null
  const max = tile.max ?? 100
  const tone = tile.critical ? 'crit' : 'normal'
  const pct = latest === null || max <= 0 ? 0 : Math.max(0, Math.min(1, latest / max))
  return (
    <div className="dashboards-gauge" data-testid={`gauge-${tile.cron}`}>
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
          {tile.display?.num ?? '—'}
        </span>
        <span className="dashboards-counter-unit">{tile.display?.after}</span>
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
    () => (tile.plot ? plotToSpark(tile.plot) : []),
    [tile],
  )

  if (tile.error) return <TileError tile={tile} />
  if (series.length === 0 || series.every(s => s.points.length < 2)) {
    return <NoData />
  }

  const tagged = series.some(s => s.tag !== '')
  const latest = !tagged && series.length === 1 ? series[0].latest : null
  const tone = tile.critical ? 'crit' : 'normal'

  return (
    <div className="dashboards-sparkline-wrap" data-testid={`sparkline-${tile.cron}`}>
      {!tagged && latest !== null && (
        <div className={`dashboards-sparkline-latest dashboards-tone-${tone}`}>
          {latest}
        </div>
      )}
      <Sparkline
        series={series}
        threshold={payload.cron.AlertThreshold}
        condition={payload.cron.AlertCondition}
        height={tile.kind === 'line' ? 160 : 80}
        showTimeAxis
        xDomain={tile.plot ? plotXDomain(tile.plot) : undefined}
      />
      {tagged && <SparkLegend series={series} />}
    </div>
  )
}

// ── Bar ────────────────────────────────────────────────────────────────────

function BarTile({ tile }: { tile: DashboardTile }) {
  if (tile.error) return <TileError tile={tile} />
  if (!tile.plot || !tile.plot.series[0]?.points.length) return <NoData />
  return (
    <div className="dashboards-bar-wrap" data-testid={`bar-${tile.cron}`}>
      <PlotBars plot={tile.plot} />
    </div>
  )
}

// A tile the chart refused to draw, such as a bar of a tagged cron or a
// percentile over a range past 7 days.
function TileError({ tile }: { tile: DashboardTile }) {
  return <div className="dashboards-tile-nodata" data-testid={`tile-error-${tile.cron}`}>{tile.error}</div>
}

// ── Table ──────────────────────────────────────────────────────────────────

export function TableTile({ tile }: { tile: DashboardTile }) {
  if (tile.error) return <TileError tile={tile} />
  // One row per tag, already in table order (numbers highest first, text by
  // tag): the cron's latest run, or its aggregate over the tile's range.
  const points = tile.points ?? []
  if (points.length === 0) return <NoData />
  const tagged = points.some(p => p.tag !== '')
  const numeric = points.every(p => p.value !== undefined && p.value !== null)

  return (
    <table className="tui-table dashboards-table" data-testid={`table-${tile.cron}`}>
      <thead>
        <tr>
          {tagged && <th>tag</th>}
          <th className={numeric ? 'dashboards-num' : undefined}>value</th>
        </tr>
      </thead>
      <tbody>
        {points.map(p => (
          <tr
            key={p.tag}
            className={p.critical ? 'dashboards-tone-crit' : undefined}
            data-testid={`table-${tile.cron}-row-${p.tag}`}
          >
            {tagged && <td>{p.tag}</td>}
            <td className={numeric ? 'dashboards-num' : undefined}>{p.text ?? (p.value ?? '')}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ── Helpers ────────────────────────────────────────────────────────────────

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

