import { useEffect, useRef } from 'react'
import { Link } from 'react-router-dom'
import { cronUrl } from '../lib/api'
import { useReportModalClosed } from '../lib/useReportModalClosed'
import { Sparkline, SparkLegend, formatSparkValue } from './Sparkline'
import { plotToSpark, plotXDomain, type ChartPlot } from '../lib/chartPlot'
import './ShellChartModal.css'

// ChartSpec mirrors bridge.ts ChartOpenSpec — kept here as a local
// duplicate so this component has no import cycle into the wasm
// bridge module.
export interface ChartSpec {
  pipeline?: string // the typed line that opened the chart
  cron?: string
  plot?: ChartPlot
  kind?: string
  title?: string
}

interface Props {
  spec: ChartSpec
  onClose: () => void
}

// ShellChartModal is the React-side render target for both
// `chart <name>` and `... | chart` typed at the browser shell. Either
// way the shell has already reduced the data to a plot: from the
// pipeline's lines, or from the cron's stored series over the shell's
// range. A cron chart also links to its cron page.
export default function ShellChartModal({ spec, onClose }: Props) {
  const title = spec.title || spec.cron || 'chart'

  // The tour completes a chart lesson when this modal goes away, but only
  // once it showed something.
  const shown = useRef(false)
  shown.current = !!spec.plot
  useReportModalClosed(spec.pipeline, shown)

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
          {spec.cron && (
            <Link
              to={`/cron/${cronUrl(spec.cron)}`}
              className="shell-chart-modal-link"
              data-testid="shell-chart-modal-watch-link"
            >
              open cron
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
          {spec.plot ? <PlotView plot={spec.plot} /> : null}
        </div>
      </div>
    </div>
  )
}

// PlotBars draws a bar plot's single series as a row per point, shared with
// dashboard bar tiles.
export function PlotBars({ plot }: { plot: ChartPlot }) {
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

function PlotView({ plot }: { plot: ChartPlot }) {
  const series = plotToSpark(plot)

  if (plot.kind === 'bar') return <PlotBars plot={plot} />

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
        xDomain={plotXDomain(plot)}
      />
      <SparkLegend series={series} />
    </>
  )
}
