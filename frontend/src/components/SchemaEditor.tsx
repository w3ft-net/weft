import { useState } from 'react'
import SchemaPreview from './SchemaPreview'
import type { ConfigureSourceRequest, Preview, ProbeFn, SourceDetection } from '../lib/api'
import { suggestFromDetection } from '../lib/api'
import ExtractionWizard from './ExtractionWizard'
import { applyPatch } from '../lib/schemaPatch'
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
  submitLabel = 'looks good, configure',
  submittingLabel = 'configuring…',
  probe,
  onSubmit,
  onPreview,
  onCancel,
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
  // Applies the finished (unsaved) config to the sample, for the preview shown
  // once the walk completes. Failures are muted; they never block configuring.
  onPreview?: (config: ConfigureSourceRequest) => Promise<Preview>
  // Called when the guided walk is cancelled; there is no manual form to fall back to.
  onCancel?: () => void
}) {
  const suggested = suggestFromDetection(detection)
  const [form, setForm] = useState<ConfigureSourceRequest>(suggested)
  const [showAll, setShowAll] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [guided, setGuided] = useState(!!probe)
  // Set once the guided walk has finished: that was the confirmation, so the
  // source is configured from it directly, like the CLI does.
  const [walked, setWalked] = useState(false)
  const [notes, setNotes] = useState<string[]>([])
  const [preview, setPreview] = useState<Preview | null>(null)

  async function submit(config: ConfigureSourceRequest = form) {
    setSubmitting(true)
    setError(null)
    try {
      await onSubmit(config)
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

  const valuesBlock = (
    <div className="psc-values" data-testid={`${testidPrefix}-values`}>
      {preview && <SchemaPreview testidPrefix={testidPrefix} preview={preview} />}
      {notes.map((n, i) => (
        <div key={i} className="psc-wiz-notice" data-testid={`${testidPrefix}-note-${i}`}>{n}</div>
      ))}
    </div>
  )

  if (walked) {
    return (
      <div className="psc-body">
        {valuesBlock}
        {error && (
          <div className="psc-error" data-testid={`${testidPrefix}-error`}>
            error: {error}
          </div>
        )}
        {(submitting || error) && (
          <div className="psc-actions">
            <button className="tui-btn tui-btn-primary" onClick={() => submit()} disabled={submitting}
              data-testid={`${testidPrefix}-submit-button`}>
              {submitting ? submittingLabel : submitLabel}
            </button>
          </div>
        )}
      </div>
    )
  }

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
            const next = applyPatch(form, patch)
            setForm(next)
            setNotes(n)
            setGuided(false)
            setWalked(true)
            onPreview?.(next).then(setPreview).catch(() => {})
            void submit(next)
          }}
          onCancel={() => onCancel?.()}
        />
      </div>
    )
  }

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

      {error && (
        <div className="psc-error" data-testid={`${testidPrefix}-error`}>
          error: {error}
        </div>
      )}

      <div className="psc-actions">
        <button
          className="tui-btn tui-btn-primary"
          onClick={() => submit()}
          disabled={submitting}
          data-testid={`${testidPrefix}-submit-button`}
        >
          {submitting ? submittingLabel : submitLabel}
        </button>
      </div>
    </div>
  )
}
