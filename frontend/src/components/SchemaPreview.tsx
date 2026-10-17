import type { Preview } from '../lib/api'
import './SchemaPreview.css'

const FIELD_LABELS: Record<string, string> = {
  timestamp: 'timestamp',
  level: 'level',
  trace_id: 'trace ID',
  span_name: 'span name',
  span_id: 'span ID',
  parent_span_id: 'parent span ID',
  duration_ns: 'duration',
}

// SchemaPreview shows sample lines with every extracted field highlighted in
// its own colour, and a key (one coloured square per field that appeared)
// underneath. A line's text content is always exactly the line.
export default function SchemaPreview({
  testidPrefix,
  preview,
}: {
  testidPrefix: string
  preview: Preview
}) {
  const fields = preview.fields ?? []
  return (
    <div className="sp" data-testid={`${testidPrefix}-preview`}>
      <div className="sp-lines">
        {preview.lines.map((segs, i) => (
          <div key={i} className="sp-line" data-testid={`${testidPrefix}-preview-line-${i}`}>
            {segs.map((s, j) =>
              s.field ? (
                <span key={j} className={`sp-hl sp-f-${s.field}`} data-field={s.field}>{s.text}</span>
              ) : (
                <span key={j}>{s.text}</span>
              ),
            )}
          </div>
        ))}
      </div>
      {fields.length > 0 && (
        <div className="sp-key" data-testid={`${testidPrefix}-preview-key`}>
          {fields.map(f => (
            <span key={f} className={`sp-f-${f}`} data-testid={`${testidPrefix}-preview-key-${f}`}>
              <span className="sp-key-swatch">■</span>{FIELD_LABELS[f] ?? f}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}
