import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { getWatchResults, type CronResult } from '../lib/api'
import { Sparkline, SparkLegend, groupResultsByTag } from './Sparkline'
import './ShellChartModal.css'

// ChartSpec mirrors bridge.ts ChartOpenSpec — kept here as a local
// duplicate so this component has no import cycle into the wasm
// bridge module.
export interface ChartSpec {
  watch?: string
  lines?: string[]
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
// the pipe form renders the captured upstream snapshot.
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

  // Esc closes, matching the label on the close button.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
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
          ) : (
            <pre
              className="shell-chart-modal-lines"
              data-testid="shell-chart-modal-pipe-lines"
            >
              {(spec.lines || []).join('\n')}
            </pre>
          )}
        </div>
      </div>
    </div>
  )
}
