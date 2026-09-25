import { useState } from 'react'
import type { ConfigureSourceRequest, SourceDetection } from '../lib/api'
import { suggestFromDetection } from '../lib/api'
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

// SchemaEditor is the shared body of the "look at a sample and confirm the
// schema" flow used by both the weftd pending-source onboarding and the
// CloudWatch add-source form. Callers hand in the sample + detection and
// get back a fully-shaped ConfigureSourceRequest on submit.
export default function SchemaEditor({
  testidPrefix,
  sample,
  detection,
  submitLabel = 'Looks good — configure',
  submittingLabel = 'Configuring…',
  onSubmit,
}: {
  testidPrefix: string
  sample: string[]
  detection: SourceDetection
  submitLabel?: string
  submittingLabel?: string
  onSubmit: (config: ConfigureSourceRequest) => Promise<void>
}) {
  const suggested = suggestFromDetection(detection)
  const [form, setForm] = useState<ConfigureSourceRequest>(suggested)
  const [showAll, setShowAll] = useState(false)
  const [showAdvanced, setShowAdvanced] = useState(false)
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
      await onSubmit(form)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  const tsCandidate = detection.TimestampCandidates?.[0]
  const tsSampleText = tsCandidate?.Sample
  const hasTimestamp = !!tsCandidate
  const visibleLines = showAll ? sample : sample.slice(0, MAX_PREVIEW_LINES)
  const hiddenCount = sample.length - visibleLines.length
  const formatLabel = FORMAT_DESCRIPTIONS[suggested.format] ?? suggested.format

  return (
    <div className="psc-body">
      <p className="psc-prose" data-testid={`${testidPrefix}-format-prose`}>
        These look like <strong>{formatLabel}</strong>.{' '}
        {hasTimestamp ? (
          <>
            We'll use{' '}
            <strong>
              {tsCandidate.JSONPath || (
                <>
                  field {tsCandidate.FieldIndex}
                  {tsCandidate.SpanFields > 1 ? `–${tsCandidate.FieldIndex + tsCandidate.SpanFields - 1}` : ''}
                </>
              )}
            </strong>
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
      <pre className="psc-sample" data-testid={`${testidPrefix}-sample`}>
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
          data-testid={`${testidPrefix}-show-all-button`}
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
              data-testid={`${testidPrefix}-format-select`}
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
              data-testid={`${testidPrefix}-timestamp-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Timestamp format</span>
            <input
              type="text"
              value={form.timestamp_format}
              onChange={e => update('timestamp_format', e.target.value)}
              placeholder="2006-01-02 15:04:05"
              data-testid={`${testidPrefix}-timestamp-format-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Level field (optional)</span>
            <input
              type="text"
              value={form.level_field}
              onChange={e => update('level_field', e.target.value)}
              placeholder="leave blank to skip"
              data-testid={`${testidPrefix}-level-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Level pattern (optional)</span>
            <input
              type="text"
              value={form.level_pattern}
              onChange={e => update('level_pattern', e.target.value)}
              placeholder="regex like ^(\\w+)/"
              data-testid={`${testidPrefix}-level-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Trace ID field (optional)</span>
            <input
              type="text"
              value={form.trace_id_field}
              onChange={e => update('trace_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-trace-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Trace ID pattern (optional)</span>
            <input
              type="text"
              value={form.trace_id_pattern}
              onChange={e => update('trace_id_pattern', e.target.value)}
              placeholder="regex like trace_id=(\\S+)"
              data-testid={`${testidPrefix}-trace-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Span name field (optional)</span>
            <input
              type="text"
              value={form.span_name_field}
              onChange={e => update('span_name_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-span-name-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Span name pattern (optional)</span>
            <input
              type="text"
              value={form.span_name_pattern}
              onChange={e => update('span_name_pattern', e.target.value)}
              placeholder="regex like span_name=(\\S+)"
              data-testid={`${testidPrefix}-span-name-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Span ID field (optional)</span>
            <input
              type="text"
              value={form.span_id_field}
              onChange={e => update('span_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-span-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Span ID pattern (optional)</span>
            <input
              type="text"
              value={form.span_id_pattern}
              onChange={e => update('span_id_pattern', e.target.value)}
              placeholder="regex like span_id=(\\S+)"
              data-testid={`${testidPrefix}-span-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Parent span ID field (optional)</span>
            <input
              type="text"
              value={form.parent_span_id_field}
              onChange={e => update('parent_span_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-parent-span-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Parent span ID pattern (optional)</span>
            <input
              type="text"
              value={form.parent_span_id_pattern}
              onChange={e => update('parent_span_id_pattern', e.target.value)}
              placeholder="regex like parent_span_id=(\\S+)"
              data-testid={`${testidPrefix}-parent-span-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Duration field (optional)</span>
            <input
              type="text"
              value={form.duration_ns_field}
              onChange={e => update('duration_ns_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-duration-ns-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Duration pattern (optional)</span>
            <input
              type="text"
              value={form.duration_ns_pattern}
              onChange={e => update('duration_ns_pattern', e.target.value)}
              placeholder="regex like duration_ns=(\\d+)"
              data-testid={`${testidPrefix}-duration-ns-pattern-input`}
            />
          </label>
        </div>
      )}

      {error && (
        <div className="psc-error" data-testid={`${testidPrefix}-error`}>
          {error}
        </div>
      )}

      <div className="psc-actions">
        <button
          className="btn-action btn-action-primary"
          onClick={submit}
          disabled={submitting}
          data-testid={`${testidPrefix}-submit-button`}
        >
          {submitting ? submittingLabel : submitLabel}
        </button>
        <button
          className="psc-textbtn"
          onClick={() => setShowAdvanced(!showAdvanced)}
          data-testid={`${testidPrefix}-advanced-button`}
        >
          {showAdvanced ? 'Hide advanced' : 'Show advanced'}
        </button>
      </div>
    </div>
  )
}
