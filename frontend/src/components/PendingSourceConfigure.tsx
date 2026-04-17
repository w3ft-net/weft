import { useState } from 'react'
import {
  configureSource,
  notifyPendingSourcesChanged,
  suggestFromDetection,
  type ConfigureSourceRequest,
  type PendingSource,
} from '../lib/api'
import './PendingSourceConfigure.css'

const MAX_PREVIEW_LINES = 20

const FORMAT_DESCRIPTIONS: Record<string, string> = {
  text: 'plain-text lines with whitespace-separated fields',
  json: 'JSON-per-line (structured events)',
  prefix_json: 'positional text fields followed by a JSON tail',
}

function Highlighted({ line, ts }: { line: string; ts?: string }) {
  if (!ts) return <>{line}</>
  const idx = line.indexOf(ts)
  if (idx < 0) return <>{line}</>
  return (
    <>
      {line.slice(0, idx)}
      <span className="psc-highlight-ts">{ts}</span>
      {line.slice(idx + ts.length)}
    </>
  )
}

export default function PendingSourceConfigure({
  source,
  onConfigured,
}: {
  source: PendingSource
  onConfigured: () => void
}) {
  const key = `${source.namespace}-${source.app}`
  const suggested = suggestFromDetection(source.detection)

  const [expanded, setExpanded] = useState(false)
  const [showAll, setShowAll] = useState(false)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [form, setForm] = useState<ConfigureSourceRequest>(suggested)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function update<K extends keyof ConfigureSourceRequest>(
    field: K,
    value: ConfigureSourceRequest[K],
  ) {
    setForm(f => ({ ...f, [field]: value }))
  }

  async function submit() {
    setSubmitting(true)
    setError(null)
    try {
      await configureSource(source.namespace, source.app, form)
      notifyPendingSourcesChanged()
      onConfigured()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  const sample = source.sample ?? []
  const tsCandidate = source.detection.TimestampCandidates?.[0]
  const tsSampleText = tsCandidate?.Sample
  const hasTimestamp = !!tsCandidate
  const visibleLines = showAll ? sample : sample.slice(0, MAX_PREVIEW_LINES)
  const hiddenCount = sample.length - visibleLines.length

  const formatLabel = FORMAT_DESCRIPTIONS[suggested.format] ?? suggested.format

  return (
    <div className="psc-row" data-testid={`pending-source-row-${key}`}>
      <button
        className="psc-header"
        onClick={() => setExpanded(!expanded)}
        data-testid={`pending-source-toggle-${key}`}
      >
        <span className="psc-app-name">{source.app}</span>
        <span className="psc-meta">
          {source.instance || 'unknown host'}
          {source.host_group ? ` · ${source.host_group}` : ''}
        </span>
        <span className="psc-chevron">{expanded ? '▾' : '▸'}</span>
      </button>

      {expanded && (
        <div className="psc-body">
          <p className="psc-prose" data-testid={`pending-source-format-prose-${key}`}>
            These look like <strong>{formatLabel}</strong>.{' '}
            {hasTimestamp ? (
              <>
                We'll use <strong>field {tsCandidate.FieldIndex}</strong>
                {tsCandidate.SpanFields > 1 ? `–${tsCandidate.FieldIndex + tsCandidate.SpanFields - 1}` : ''}
                {' '}as the timestamp
                {tsCandidate.Format ? (
                  <>
                    {' '}(<code>{tsCandidate.Format}</code>)
                  </>
                ) : null}
                .
              </>
            ) : (
              <>No timestamp detected — we'll use <strong>ingestion time</strong>.</>
            )}
          </p>

          <div className="psc-sample-label">
            Sample ({visibleLines.length} of {sample.length} line{sample.length === 1 ? '' : 's'})
          </div>
          <pre className="psc-sample" data-testid={`pending-source-sample-${key}`}>
            {visibleLines.map((line, i) => (
              <div key={i} className="psc-sample-line">
                <Highlighted line={line} ts={tsSampleText} />
              </div>
            ))}
          </pre>
          {hiddenCount > 0 && (
            <button
              className="psc-textbtn"
              onClick={() => setShowAll(true)}
              data-testid={`pending-source-show-all-${key}-button`}
            >
              Show {hiddenCount} more line{hiddenCount === 1 ? '' : 's'}
            </button>
          )}

          {showAdvanced && (
            <div className="psc-advanced">
              <label className="psc-field">
                <span className="psc-label">Format</span>
                <select
                  value={form.format}
                  onChange={e => update('format', e.target.value)}
                  data-testid={`pending-source-format-${key}-select`}
                >
                  <option value="text">text</option>
                  <option value="json">json</option>
                  <option value="prefix_json">prefix_json</option>
                </select>
              </label>

              <label className="psc-field">
                <span className="psc-label">Timestamp field</span>
                <input
                  type="text"
                  value={form.timestamp_field}
                  onChange={e => update('timestamp_field', e.target.value)}
                  placeholder="blank = ingest time"
                  data-testid={`pending-source-timestamp-field-${key}-input`}
                />
              </label>

              <label className="psc-field">
                <span className="psc-label">Timestamp format</span>
                <input
                  type="text"
                  value={form.timestamp_format}
                  onChange={e => update('timestamp_format', e.target.value)}
                  placeholder="2006-01-02 15:04:05"
                  data-testid={`pending-source-timestamp-format-${key}-input`}
                />
              </label>

              <label className="psc-field">
                <span className="psc-label">Level field (optional)</span>
                <input
                  type="text"
                  value={form.level_field}
                  onChange={e => update('level_field', e.target.value)}
                  placeholder="leave blank to skip"
                  data-testid={`pending-source-level-field-${key}-input`}
                />
              </label>

              <label className="psc-field">
                <span className="psc-label">Level pattern (optional)</span>
                <input
                  type="text"
                  value={form.level_pattern}
                  onChange={e => update('level_pattern', e.target.value)}
                  placeholder="regex like ^(\\w+)/"
                  data-testid={`pending-source-level-pattern-${key}-input`}
                />
              </label>
            </div>
          )}

          {error && (
            <div className="psc-error" data-testid={`pending-source-error-${key}`}>
              {error}
            </div>
          )}

          <div className="psc-actions">
            <button
              className="btn-action btn-action-primary"
              onClick={submit}
              disabled={submitting}
              data-testid={`pending-source-submit-${key}-button`}
            >
              {submitting ? 'Configuring…' : 'Looks good — configure'}
            </button>
            <button
              className="psc-textbtn"
              onClick={() => setShowAdvanced(!showAdvanced)}
              data-testid={`pending-source-advanced-${key}-button`}
            >
              {showAdvanced ? 'Hide advanced' : 'Show advanced'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
