import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { getWatchResults, type CronResult } from '../lib/api'
import { Sparkline, SparkLegend, groupResultsByTag, formatSparkValue } from './Sparkline'
import { plotToSpark, type ChartPlot } from '../lib/chartPlot'
import './ShellChartModal.css'

// ChartSpec mirrors bridge.ts ChartOpenSpec — kept here as a local
// duplicate so this component has no import cycle into the wasm
// bridge module.
export interface ChartSpec {
  watch?: string
  plot?: ChartPlot
  kind?: string
  title?: string
  limit?: number
}

interface Props {
  spec: ChartSpec
  onClose: () => void
}

// ShellChartModal is the React-side render target for both
// `chart <name>` and `... | chart` typed at the browser shell. The
// watch form re-fetches results from /api/watches/<name>/results
// (matching the dashboard tile flow) so the modal stays live;
// the pipe form draws the plot the shell computed from its pipeline.
export default function ShellChartModal({ spec, onClose }: Props) {
  const isWatch = !!spec.watch
  const title = spec.title || spec.watch || 'chart'

  const [results, setResults] = useState<CronResult[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!isWatch || !spec.watch) return
    let cancelled = false
    getWatchResults(spec.watch)
      .then(r => { if (!cancelled) setResults(r) })
      .catch(e => { if (!cancelled) setError(String(e)) })
    return () => { cancelled = true }
  }, [spec.watch, isWatch])

  // Esc closes, matching the label on the close button. Capture phase:
  // focus normally sits in the terminal behind the backdrop, and xterm
  // consumes keys before they bubble to window.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])

  const series = isWatch && results ? groupResultsByTag(results) : []

  return (
    <div
      className="shell-chart-modal-backdrop"
      onClick={onClose}
      data-testid="shell-chart-modal"
    >
      <div
        className="shell-chart-modal-panel"
        onClick={e => e.stopPropagation()}
      >
        <header className="shell-chart-modal-header">
          <h2 className="shell-chart-modal-title">{title}</h2>
          {isWatch && spec.watch && (
            <Link
              to={`/watch/${encodeURIComponent(spec.watch)}`}
              className="shell-chart-modal-link"
              data-testid="shell-chart-modal-watch-link"
            >
              open watch
            </Link>
          )}
          <button
            className="shell-chart-modal-close"
            onClick={onClose}
            data-testid="shell-chart-modal-close-button"
            aria-label="Close"
          >
            esc
          </button>
        </header>

        <div className="shell-chart-modal-body">
          {isWatch ? (
            error ? (
              <div className="shell-chart-modal-error">{error}</div>
            ) : results === null ? (
              <div className="shell-chart-modal-loading">loading…</div>
            ) : series.length === 0 ? (
              <div className="shell-chart-modal-empty">no numeric data yet</div>
            ) : (
              <>
                <Sparkline series={series} height={180} showTimeAxis />
                <SparkLegend series={series} />
              </>
            )
          ) : spec.plot ? (
            <PlotView plot={spec.plot} />
          ) : null}
        </div>
      </div>
    </div>
  )
}

function PlotView({ plot }: { plot: ChartPlot }) {
  const series = plotToSpark(plot)

  if (plot.kind === 'bar') {
    const points = plot.series[0]?.points ?? []
    const max = Math.max(...points.map(p => p.y), 0)
    return (
      <div className="shell-chart-modal-bars" data-testid="shell-chart-modal-bars">
        {points.map(p => (
          <div key={p.label} className="shell-chart-modal-bar-row" data-testid={`shell-chart-modal-bar-${p.label}`}>
            <span className="shell-chart-modal-bar-label">{p.label}</span>
            <span className="shell-chart-modal-bar-track">
              <span className="shell-chart-modal-bar-fill" style={{ width: `${max > 0 ? (p.y / max) * 100 : 0}%` }} />
            </span>
            <span className="shell-chart-modal-bar-value">{formatSparkValue(p.y)}</span>
          </div>
        ))}
      </div>
    )
  }

  // A line needs two points. Everything landing in one bucket is a real
  // result (the window was too short for this bucket size), so say so rather
  // than show an empty plot.
  if (!series.some(s => s.points.length >= 2)) {
    return (
      <div className="shell-chart-modal-empty" data-testid="shell-chart-modal-single-value">
        {series.map(s => (
          <div key={s.tag}>{s.tag ? `${s.tag}: ` : ''}{s.latest}</div>
        ))}
        <div>
          Only one data point — widen the range or use a smaller --bucket to see a line.
        </div>
      </div>
    )
  }

  return (
    <>
      <Sparkline
        series={series}
        height={plot.kind === 'sparkline' ? 48 : 180}
        showTimeAxis={plot.timeX && plot.kind !== 'sparkline'}
      />
      <SparkLegend series={series} />
    </>
  )
}
