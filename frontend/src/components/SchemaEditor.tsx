import { useState } from 'react'
import type { ConfigureSourceRequest, ProbeFn, SourceDetection } from '../lib/api'
import { suggestFromDetection } from '../lib/api'
import ExtractionWizard from './ExtractionWizard'
import { applyPatch, describeValue } from '../lib/schemaPatch'
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

const VALUE_ROWS: { key: string; label: string }[] = [
  { key: 'timestamp', label: 'timestamp' },
  { key: 'level', label: 'log level' },
  { key: 'trace_id', label: 'trace ID' },
  { key: 'span_name', label: 'span name' },
  { key: 'span_id', label: 'span ID' },
  { key: 'parent_span_id', label: 'parent span ID' },
  { key: 'duration_ns', label: 'span duration' },
]

// SchemaEditor is the shared body of the "look at a sample and confirm the
// schema" flow used by both the weftd pending-source onboarding and the
// CloudWatch add-source form. Callers hand in the sample + detection and
// get back a fully-shaped ConfigureSourceRequest on submit.
export default function SchemaEditor({
  testidPrefix,
  sample,
  detection,
  submitLabel = 'looks good, configure',
  submittingLabel = 'configuring…',
  probe,
  onSubmit,
}: {
  testidPrefix: string
  sample: string[]
  detection: SourceDetection
  submitLabel?: string
  submittingLabel?: string
  // When given, each extractable value is walked through the decision tree
  // (ExtractionWizard) instead of being taken from detection's first guess.
  probe?: ProbeFn
  onSubmit: (config: ConfigureSourceRequest) => Promise<void>
}) {
  const suggested = suggestFromDetection(detection)
  const [form, setForm] = useState<ConfigureSourceRequest>(suggested)
  const [showAll, setShowAll] = useState(false)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [guided, setGuided] = useState(!!probe)
  const [notes, setNotes] = useState<string[]>([])

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

  if (guided && probe) {
    const spansDetected = [
      detection.SpanNameCandidates, detection.SpanIDCandidates,
      detection.ParentSpanIDCandidates, detection.DurationNsCandidates,
    ].some(c => (c?.length ?? 0) > 0)
    return (
      <div className="psc-body">
        <ExtractionWizard
          testidPrefix={testidPrefix}
          sample={sample}
          base={{
            envelope: form.envelope ?? '',
            format: form.format,
            field_separator: '',
            json_start_field: form.json_start_field ?? 0,
          }}
          probe={probe}
          spansDetected={spansDetected}
          onComplete={(patch, n) => {
            setForm(f => applyPatch(f, patch))
            setNotes(n)
            setGuided(false)
          }}
          onCancel={() => setGuided(false)}
        />
      </div>
    )
  }

  return (
    <div className="psc-body">
      {probe && (
        <div className="psc-values" data-testid={`${testidPrefix}-values`}>
          <div className="psc-sample-label">values to extract</div>
          {VALUE_ROWS.map(r => (
            <div key={r.key} className="psc-value-row" data-testid={`${testidPrefix}-value-${r.key}`}>
              <span className="psc-label">{r.label}</span>
              <span className="psc-value-rule">{describeValue(form, r.key)}</span>
            </div>
          ))}
          {notes.map((n, i) => (
            <div key={i} className="psc-wiz-notice" data-testid={`${testidPrefix}-note-${i}`}>{n}</div>
          ))}
          <button className="psc-textbtn" onClick={() => setGuided(true)}
            data-testid={`${testidPrefix}-rewalk-button`}>walk through the values again</button>
        </div>
      )}
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
        <span className="psc-prompt">$</span> head -{visibleLines.length} sample.log
        <span className="psc-sample-count">{visibleLines.length}/{sample.length} line{sample.length === 1 ? '' : 's'}</span>
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
          show {hiddenCount} more line{hiddenCount === 1 ? '' : 's'}
        </button>
      )}

      {showAdvanced && (
        <div className="psc-advanced">
          <label className="psc-field">
            <span className="psc-label">format:</span>
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
            <span className="psc-label">timestamp field:</span>
            <input
              type="text"
              value={form.timestamp_field}
              onChange={e => update('timestamp_field', e.target.value)}
              placeholder="blank = ingest time"
              data-testid={`${testidPrefix}-timestamp-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">timestamp format:</span>
            <input
              type="text"
              value={form.timestamp_format}
              onChange={e => update('timestamp_format', e.target.value)}
              placeholder="2006-01-02 15:04:05"
              data-testid={`${testidPrefix}-timestamp-format-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">level field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.level_field}
              onChange={e => update('level_field', e.target.value)}
              placeholder="leave blank to skip"
              data-testid={`${testidPrefix}-level-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">level pattern<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.level_pattern}
              onChange={e => update('level_pattern', e.target.value)}
              placeholder="regex like ^(\\w+)/"
              data-testid={`${testidPrefix}-level-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">trace id field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.trace_id_field}
              onChange={e => update('trace_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-trace-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">trace id pattern<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.trace_id_pattern}
              onChange={e => update('trace_id_pattern', e.target.value)}
              placeholder="regex like trace_id=(\\S+)"
              data-testid={`${testidPrefix}-trace-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">span name field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.span_name_field}
              onChange={e => update('span_name_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-span-name-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">span name pattern<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.span_name_pattern}
              onChange={e => update('span_name_pattern', e.target.value)}
              placeholder="regex like span_name=(\\S+)"
              data-testid={`${testidPrefix}-span-name-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">span id field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.span_id_field}
              onChange={e => update('span_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-span-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">span id pattern<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.span_id_pattern}
              onChange={e => update('span_id_pattern', e.target.value)}
              placeholder="regex like span_id=(\\S+)"
              data-testid={`${testidPrefix}-span-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">parent span id field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.parent_span_id_field}
              onChange={e => update('parent_span_id_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-parent-span-id-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">parent span id pattern<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.parent_span_id_pattern}
              onChange={e => update('parent_span_id_pattern', e.target.value)}
              placeholder="regex like parent_span_id=(\\S+)"
              data-testid={`${testidPrefix}-parent-span-id-pattern-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">duration field<span className="psc-opt">(opt)</span>:</span>
            <input
              type="text"
              value={form.duration_ns_field}
              onChange={e => update('duration_ns_field', e.target.value)}
              placeholder="field index, or .path for JSON"
              data-testid={`${testidPrefix}-duration-ns-field-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">duration pattern<span className="psc-opt">(opt)</span>:</span>
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
          error: {error}
        </div>
      )}

      <div className="psc-actions">
        <button
          className="tui-btn tui-btn-primary"
          onClick={submit}
          disabled={submitting}
          data-testid={`${testidPrefix}-submit-button`}
        >
          {submitting ? submittingLabel : submitLabel}
        </button>
        <button
          className="tui-btn"
          onClick={() => setShowAdvanced(!showAdvanced)}
          data-testid={`${testidPrefix}-advanced-button`}
        >
          {showAdvanced ? 'hide advanced' : 'advanced…'}
        </button>
      </div>
    </div>
  )
}
