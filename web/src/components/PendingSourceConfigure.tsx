import { useEffect, useState } from 'react'
import {
  configureSource,
  notifyPendingSourcesChanged,
  previewSource,
  probeSource,
  resampleSource,
  LARGER_SAMPLE_LINES,
  type PendingSource,
} from '../lib/api'
import SchemaEditor from './SchemaEditor'
import './PendingSourceConfigure.css'

function timeAgo(rfc3339: string, now: number): string {
  const secs = Math.max(0, Math.round((now - Date.parse(rfc3339)) / 1000))
  if (secs < 60) return `${secs}s ago`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`
  return `${Math.floor(secs / 86400)}d ago`
}

export default function PendingSourceConfigure({
  source,
  onConfigured,
}: {
  source: PendingSource
  onConfigured: () => void
}) {
  const key = `${source.namespace}-${source.service}`
  const [expanded, setExpanded] = useState(false)
  const [resampling, setResampling] = useState(false)
  const [resampleError, setResampleError] = useState<string | null>(null)

  // Send the source back to sampling: the first batch (process startup,
  // etc.) may not show everything worth extracting. Fresh lines only.
  async function collectNewSample() {
    setResampling(true)
    setResampleError(null)
    try {
      await resampleSource(source.namespace, source.service)
      notifyPendingSourcesChanged()
    } catch (e) {
      setResampleError(e instanceof Error ? e.message : String(e))
    } finally {
      setResampling(false)
    }
  }

  const resampleButton = (
    <div className="psc-actions">
      <button
        className="tui-btn"
        onClick={collectNewSample}
        disabled={resampling}
        data-testid={`pending-source-resample-button-${key}`}
      >
        {resampling ? 'requesting…' : 'collect a new sample'}
      </button>
      {resampleError && (
        <span className="psc-error" data-testid={`pending-source-resample-error-${key}`}>
          {resampleError}
        </span>
      )}
    </div>
  )
  // Tick so "last line Ns ago" advances between polls.
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (source.status !== 'sampling') return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [source.status])

  // A still-sampling source has no full sample yet — it can't be
  // configured. Show live progress and the lines seen so far instead of the
  // schema editor, so the user can see it filling.
  if (source.status === 'sampling') {
    const lines = source.sample ?? []
    const pct = source.sample_target > 0
      ? Math.min(100, Math.round((source.sample_collected / source.sample_target) * 100))
      : 0
    return (
      <div className="psc-row" data-testid={`pending-source-row-${key}`}>
        <button
          className="psc-header"
          onClick={() => setExpanded(!expanded)}
          aria-expanded={expanded}
          data-testid={`pending-source-sampling-toggle-${key}`}
        >
          <span className="psc-chevron">{expanded ? '▾' : '▸'}</span>
          <span className="psc-service-name">{source.service}</span>
          <span className="tui-tag tui-tag-warn" data-testid={`pending-source-sampling-${key}`}>
            sampling {source.sample_collected}/{source.sample_target}
          </span>
          <span className="psc-meta">
            <span
              className="psc-meter"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={source.sample_target}
              aria-valuenow={source.sample_collected}
              data-testid={`pending-source-meter-${key}`}
            >
              <span className="psc-meter-fill" style={{ width: `${pct}%` }} />
            </span>
            {' '}{pct}% · {source.sample_collected === 0 && source.sample_updated_at
              ? 'waiting for new lines'
              : <>last line {source.sample_updated_at ? timeAgo(source.sample_updated_at, now) : 'not yet'}</>}
          </span>
        </button>
        {expanded && (
          <pre className="psc-preview" data-testid={`pending-source-preview-${key}`}>
            {lines.length > 0
              ? lines.slice(-10).join('\n')
              : 'No lines yet — waiting for this source to emit.'}
          </pre>
        )}
      </div>
    )
  }

  return (
    <div className="psc-row" data-testid={`pending-source-row-${key}`}>
      <button
        className="psc-header"
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
        data-testid={`pending-source-toggle-${key}`}
      >
        <span className="psc-chevron">{expanded ? '▾' : '▸'}</span>
        <span className="psc-service-name">{source.service}</span>
        <span className="tui-tag tui-tag-hi">pending</span>
        <span className="psc-meta">
          {source.instance || 'unknown host'}
          {source.host_group ? ` (${source.host_group})` : ''}
        </span>
      </button>

      {expanded && resampleButton}
      {expanded && (
        <SchemaEditor
          testidPrefix={`pending-source-${key}`}
          sample={source.sample ?? []}
          probe={req => probeSource(source.namespace, source.service, req)}
          detection={source.detection}
          onPreview={config => previewSource(source.namespace, source.service, config)}
          onCancel={() => setExpanded(false)}
          onLargerSample={async () => {
            await resampleSource(source.namespace, source.service, LARGER_SAMPLE_LINES)
            notifyPendingSourcesChanged()
            setExpanded(false)
          }}
          onSubmit={async (config) => {
            await configureSource(source.namespace, source.service, config)
            notifyPendingSourcesChanged()
            onConfigured()
          }}
        />
      )}
    </div>
  )
}
