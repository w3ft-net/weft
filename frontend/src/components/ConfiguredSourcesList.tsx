import { useState } from 'react'
import {
  deleteSource,
  resampleSource,
  getSourceSchema,
  type SourceInfo,
  type SourceSchema,
} from '../lib/api'
import SchemaPreview from './SchemaPreview'
import './PendingSourceConfigure.css'

// ConfiguredSourcesList renders the configured sources for a single
// namespace panel. Caller supplies the already-filtered list and a
// change handler so the parent can refetch after a delete.
export default function ConfiguredSourcesList({
  namespace,
  sources,
  onChanged,
}: {
  namespace: string
  sources: SourceInfo[]
  onChanged: () => void
}) {
  // service → fetched schema (null while loading; key absent when hidden).
  const [schemas, setSchemas] = useState<Record<string, SourceSchema | null>>({})
  const [busy, setBusy] = useState<Record<string, boolean>>({})

  if (sources.length === 0) return null

  async function handleDelete(service: string) {
    if (!confirm(`Remove source ${service}? Already-ingested rows stay in the shards; only future polling stops.`)) {
      return
    }
    try {
      await deleteSource(namespace, service)
      onChanged()
    } catch (e) {
      alert(`Remove failed: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  async function loadSchema(service: string) {
    setSchemas(s => ({ ...s, [service]: null }))
    try {
      const sch = await getSourceSchema(namespace, service)
      setSchemas(s => ({ ...s, [service]: sch }))
    } catch (e) {
      alert(`Load schema failed: ${e instanceof Error ? e.message : String(e)}`)
      setSchemas(s => {
        const next = { ...s }
        delete next[service]
        return next
      })
    }
  }

  function toggleSchema(service: string) {
    if (service in schemas) {
      setSchemas(s => {
        const next = { ...s }
        delete next[service]
        return next
      })
      return
    }
    loadSchema(service)
  }

  async function handleResample(service: string) {
    if (!confirm(`Resample ${service}? Ingestion keeps running on the current schema; a fresh sample appears for review once the agent has collected enough lines.`)) {
      return
    }
    setBusy(b => ({ ...b, [service]: true }))
    try {
      await resampleSource(namespace, service)
      // Refresh an already-open schema panel so the "resample in
      // progress" state shows immediately.
      if (service in schemas) await loadSchema(service)
      onChanged()
    } catch (e) {
      alert(`Resample failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(b => ({ ...b, [service]: false }))
    }
  }

  return (
    <div
      className="csl-list"
      data-testid={`configured-sources-${namespace}-list`}
    >
      {sources.map(s => {
        const sch = schemas[s.service]
        const open = s.service in schemas
        return (
          <div
            key={s.service}
            className="csl-row"
            data-testid={`configured-source-row-${s.service}`}
          >
            <div className="csl-row-main">
              <span className="csl-service">{s.service}</span>
              <span className={`tui-tag csl-badge csl-badge-${s.source_type}`}>{s.source_type}</span>
              {s.source_type === 'kinesis' && (
                <span className="csl-meta">
                  {s.log_group} <span className="csl-sep">{s.region}</span> <span className="csl-sep">firehose push</span>
                </span>
              )}
            </div>
            <div className="csl-row-actions">
              <button
                className="tui-btn"
                onClick={() => toggleSchema(s.service)}
                data-testid={`configured-source-${s.service}-viewschema-button`}
              >
                {open ? 'hide schema' : 'schema'}
              </button>
              <button
                className="tui-btn"
                disabled={busy[s.service]}
                onClick={() => handleResample(s.service)}
                data-testid={`configured-source-${s.service}-resample-button`}
              >
                {busy[s.service] ? 'resampling…' : 'resample'}
              </button>
              <button
                className="tui-btn tui-btn-danger"
                onClick={() => handleDelete(s.service)}
                data-testid={`configured-source-${s.service}-remove-button`}
              >
                remove
              </button>
            </div>
            {open && (
              <div
                className="csl-schema-panel"
                data-testid={`configured-source-${s.service}-schema-panel`}
              >
                {sch === null && <span className="csl-meta csl-meta-dim">loading…</span>}
                {sch && sch.status === 'configured' && (
                  <>
                    {sch.resample_requested && (
                      <div className="csl-meta csl-meta-dim">
                        <span className="tui-tag tui-tag-warn">resampling {sch.sample_collected ?? 0}/{sch.sample_target ?? 0}</span> ingestion continues on the current schema
                      </div>
                    )}
                    {sch.sdk_note && (
                      <div className="csl-meta csl-meta-dim" data-testid={`configured-source-${s.service}-schema-sdk-note`}>
                        {sch.sdk_note}
                      </div>
                    )}
                    {sch.preview ? (
                      <SchemaPreview testidPrefix={`configured-source-${s.service}-schema`} preview={sch.preview} />
                    ) : (
                      <div className="csl-meta csl-meta-dim" data-testid={`configured-source-${s.service}-schema-note`}>
                        {sch.preview_note || 'no preview available'}
                      </div>
                    )}
                  </>
                )}
                {sch && sch.status === 'sampling' && (
                  <span className="csl-meta">
                    <span className="tui-tag tui-tag-warn">sampling {sch.sample_collected ?? 0}/{sch.sample_target ?? 0}</span> {s.service} hasn’t emitted enough lines to sample yet.
                  </span>
                )}
                {sch && sch.status === 'pending' && (
                  <span className="csl-meta">
                    <span className="tui-tag tui-tag-hi">pending</span> a fresh sample is awaiting configuration ({sch.sample_collected ?? 0}/{sch.sample_target ?? 0} lines).
                  </span>
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
