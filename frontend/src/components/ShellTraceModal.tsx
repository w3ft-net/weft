import { useEffect, useMemo, useRef, useState } from 'react'
import { fetchPipelineLines } from '../wasm/network'
import { useReportModalClosed } from '../lib/useReportModalClosed'
import { parseTraceRows, renderSpanRollup, type TraceRow, type SpanRollupRow, type SpanOccurrence } from '../wasm/traceExports'
import { traceColorFor } from '../lib/traceColors'
import './ShellTraceModal.css'

// TraceSpec mirrors bridge.ts TraceOpenSpec — kept here as a local
// duplicate so this component has no import cycle into the wasm
// bridge module (same pattern as ShellChartModal's ChartSpec).
export interface TraceSpec {
  pipeline: string
  title: string
  mode: 'gap' | 'rollup' | 'aggregate'
}

interface Props {
  spec: TraceSpec
  onClose: () => void
}

// formatDurationNs renders a nanosecond count roughly the way Go's
// time.Duration.String() would (e.g. "1.5ms", "669µs", "2s") — not
// byte-identical, just legible at the same units a Go-side ANSI
// render would have used.
function formatDurationNs(ns: number): string {
  if (!Number.isFinite(ns) || ns <= 0) return '0s'
  const trim = (n: number) => (Math.round(n * 1000) / 1000).toString()
  if (ns < 1_000) return `${ns}ns`
  if (ns < 1_000_000) return `${trim(ns / 1_000)}µs`
  if (ns < 1_000_000_000) return `${trim(ns / 1_000_000)}ms`
  return `${trim(ns / 1_000_000_000)}s`
}

// ShellTraceModal is the React-side render target for `trace <id>`
// (Mode "gap"), `trace <id> --spans` (Mode "rollup", Phase 1), and
// `trace spans [name]` (Mode "aggregate", Phase 2) typed at the
// browser shell — the browser half of docs/plans/span-display.md's
// vim-nav list->samples UX, rendered as a React table instead of an
// ANSI shellcore.View. It re-fetches spec.pipeline itself (there's no
// captured upstream snapshot the way `... | chart` has, since `trace`
// is always a pipeline's source command) and hands the raw rows to
// the wasmParseTraceRows/wasmRenderSpanRollup exports so the
// self-time/percentile/gap math stays in theme, not reimplemented
// here.
export default function ShellTraceModal({ spec, onClose }: Props) {
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [lines, setLines] = useState<string[] | null>(null)
  const [level, setLevel] = useState<'list' | 'samples'>('list')
  const [focusIdx, setFocusIdx] = useState<number | null>(null)

  // The tour waits for this modal to go away before moving on, but only for
  // a trace that loaded.
  const shown = useRef(false)
  shown.current = !loading && !error && lines !== null
  useReportModalClosed(spec.pipeline, shown)

  // Esc closes, matching the [x] button. Capture phase: focus normally sits
  // in the terminal behind the backdrop, and xterm must not see the key.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])

  useEffect(() => {
    setLoading(true)
    setError(null)
    setLines(null)
    setLevel('list')
    setFocusIdx(null)
    const controller = new AbortController()
    fetchPipelineLines(spec.pipeline, controller.signal)
      .then(ls => { setLines(ls); setLoading(false) })
      .catch(e => {
        // AbortError fires from our own cleanup below — expected under
        // StrictMode's mount/cleanup/remount dev cycle (the first
        // effect's fetch is deliberately cancelled once the second
        // one starts) and whenever the user retypes a trace command
        // before the previous fetch resolves. Neither is a real
        // failure, so don't flash an error the in-flight replacement
        // request is about to overwrite anyway.
        if ((e as Error)?.name === 'AbortError') return
        setError(String((e as Error)?.message || e))
        setLoading(false)
      })
    return () => controller.abort()
  }, [spec.pipeline])

  const gapRows = useMemo<TraceRow[]>(() => {
    if (spec.mode !== 'gap' || !lines) return []
    try {
      return parseTraceRows(lines)
    } catch (e) {
      setError(String((e as Error)?.message || e))
      return []
    }
  }, [spec.mode, lines])

  const rollupRows = useMemo<SpanRollupRow[]>(() => {
    if (spec.mode === 'gap' || !lines) return []
    try {
      return renderSpanRollup(lines)
    } catch (e) {
      setError(String((e as Error)?.message || e))
      return []
    }
  }, [spec.mode, lines])

  const singleTrace = spec.mode === 'rollup'
  const maxGapNs = useMemo(
    () => gapRows.reduce((m, r) => (r.IsData && r.GapNs > m ? r.GapNs : m), 0),
    [gapRows],
  )
  const barAgainst = useMemo(() => {
    let max = 0
    for (const r of rollupRows) {
      const v = singleTrace ? r.SelfTotal : r.Avg
      if (v > max) max = v
    }
    return max
  }, [rollupRows, singleTrace])

  const focusRow = focusIdx != null ? rollupRows[focusIdx] : null

  function openSamples(i: number) {
    setFocusIdx(i)
    setLevel('samples')
  }

  return (
    <div
      className="shell-trace-modal-backdrop"
      onClick={onClose}
      data-testid="shell-trace-modal"
    >
      <div className="shell-trace-modal-panel" onClick={e => e.stopPropagation()}>
        <header className="shell-trace-modal-header">
          <h2 className="shell-trace-modal-title">
            {level === 'samples' && focusRow ? `${spec.title} › ${focusRow.Name}` : spec.title}
          </h2>
          {level === 'samples' && (
            <button
              className="shell-trace-modal-back tui-btn"
              onClick={() => { setLevel('list'); setFocusIdx(null) }}
              data-testid="shell-trace-modal-back-button"
            >
              back
            </button>
          )}
          <button
            className="shell-trace-modal-close"
            onClick={onClose}
            data-testid="shell-trace-modal-close-button"
            aria-label="Close"
          >
            [x]
          </button>
        </header>

        <div className="shell-trace-modal-body">
          {loading ? (
            <div className="shell-trace-modal-loading tui-cursor">loading</div>
          ) : error ? (
            <div className="shell-trace-modal-error" data-testid="shell-trace-modal-error">{error}</div>
          ) : spec.mode === 'gap' ? (
            <GapView rows={gapRows} maxGapNs={maxGapNs} />
          ) : level === 'list' ? (
            <RollupListView rows={rollupRows} singleTrace={singleTrace} barAgainst={barAgainst} onSelect={openSamples} />
          ) : (
            focusRow && <SamplesView row={focusRow} />
          )}
        </div>
      </div>
    </div>
  )
}

// stripAnsi removes SGR escape sequences from server text meant for a
// real terminal. Passthrough (non-data) trace rows carry the server's
// own ANSI-colored summary/placeholder text verbatim (see
// compute/local.go's emitTraceTagged, which wraps "# N line(s)..."
// and "# no lines found" in theme.Palette().Comment/Reset for the
// CLI's scrollback) — real data rows never contain escapes, so this
// only ever has work to do on those two synthetic lines.
function stripAnsi(s: string): string {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;]*m/g, '')
}

function GapView({ rows, maxGapNs }: { rows: TraceRow[]; maxGapNs: number }) {
  if (rows.length === 0) {
    return <div className="shell-trace-modal-empty">#&nbsp;no lines found</div>
  }
  return (
    <div className="shell-trace-gap-list" data-testid="shell-trace-gap-list">
      {rows.map((r, i) => {
        if (!r.IsData) {
          return (
            <div key={i} className="shell-trace-gap-passthrough" data-testid={`shell-trace-gap-passthrough-${i}`}>
              {stripAnsi(r.Raw)}
            </div>
          )
        }
        const pct = maxGapNs > 0 ? Math.min(100, (r.GapNs / maxGapNs) * 100) : 0
        return (
          <div key={i} className="shell-trace-gap-row" data-testid={`shell-trace-gap-row-${i}`}>
            <div className="shell-trace-gap-bar-col">
              <div className="shell-trace-gap-bar" style={{ width: `${pct}%` }} />
            </div>
            <div className="shell-trace-gap-badge" style={{ color: traceColorFor(r.ColorIdx) }}>
              {r.Label}
            </div>
            <div className="shell-trace-gap-message">{r.Message}</div>
          </div>
        )
      })}
    </div>
  )
}

function RollupListView({
  rows, singleTrace, barAgainst, onSelect,
}: {
  rows: SpanRollupRow[]
  singleTrace: boolean
  barAgainst: number
  onSelect: (i: number) => void
}) {
  if (rows.length === 0) {
    return <div className="shell-trace-modal-empty">#&nbsp;no spans found</div>
  }
  return (
    <table className="shell-trace-rollup-table" data-testid="shell-trace-rollup-table">
      <thead>
        <tr>
          <th>NAME</th>
          <th></th>
          <th>COUNT</th>
          {singleTrace ? (
            <th>TOTAL</th>
          ) : (
            <>
              <th>AVG</th>
              <th>P50</th>
              <th>P99</th>
              <th>MIN</th>
              <th>MAX</th>
            </>
          )}
          <th>ERRORS</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r, i) => {
          const barValue = singleTrace ? r.SelfTotal : r.Avg
          const pct = barAgainst > 0 ? Math.min(100, (barValue / barAgainst) * 100) : 0
          return (
            <tr
              key={r.Name}
              className="shell-trace-rollup-row"
              onClick={() => onSelect(i)}
              data-testid={`shell-trace-rollup-row-${r.Name}`}
            >
              <td className="shell-trace-rollup-name">{r.Name}</td>
              <td className="shell-trace-rollup-bar-cell">
                <div className="shell-trace-rollup-bar" style={{ width: `${pct}%` }} />
              </td>
              <td className="shell-trace-rollup-num">{r.Occurrences?.length ?? 0}</td>
              {singleTrace ? (
                <td className="shell-trace-rollup-num">{formatDurationNs(r.SelfTotal)}</td>
              ) : (
                <>
                  <td className="shell-trace-rollup-num">{formatDurationNs(r.Avg)}</td>
                  <td className="shell-trace-rollup-num">{formatDurationNs(r.P50)}</td>
                  <td className="shell-trace-rollup-num">{formatDurationNs(r.P99)}</td>
                  <td className="shell-trace-rollup-num">{formatDurationNs(r.Min)}</td>
                  <td className="shell-trace-rollup-num">{formatDurationNs(r.Max)}</td>
                </>
              )}
              <td className={`shell-trace-rollup-num${r.Errors > 0 ? ' shell-trace-rollup-errors' : ''}`}>
                {r.Errors}
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

function SamplesView({ row }: { row: SpanRollupRow }) {
  const occs: SpanOccurrence[] = row.Occurrences ?? []
  if (occs.length === 0) {
    return <div className="shell-trace-modal-empty">#&nbsp;no occurrences</div>
  }
  return (
    <table className="shell-trace-samples-table" data-testid="shell-trace-samples-table">
      <thead>
        <tr>
          <th>TRACE ID</th>
          <th>LABEL</th>
          <th>DURATION</th>
          <th>SELF</th>
          <th>STATUS</th>
        </tr>
      </thead>
      <tbody>
        {occs.map((o, i) => (
          <tr key={i} className="shell-trace-samples-row" data-testid={`shell-trace-samples-row-${i}`}>
            <td className="shell-trace-samples-traceid">{o.TraceID}</td>
            <td>{o.Label}</td>
            <td className="shell-trace-rollup-num">{formatDurationNs(o.Duration)}</td>
            <td className="shell-trace-rollup-num">{formatDurationNs(o.SelfDuration)}</td>
            <td className={o.Status === 'error' ? 'shell-trace-rollup-errors' : ''}>{o.Status}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
