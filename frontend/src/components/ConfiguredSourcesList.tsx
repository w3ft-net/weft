import { useState } from 'react'
import {
  deleteSource,
  resampleSource,
  getSourceSchema,
  type SourceInfo,
  type SourceSchema,
} from '../lib/api'
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
  // app → fetched schema (null while loading; key absent when hidden).
  const [schemas, setSchemas] = useState<Record<string, SourceSchema | null>>({})
  const [busy, setBusy] = useState<Record<string, boolean>>({})

  if (sources.length === 0) return null

  async function handleDelete(app: string) {
    if (!confirm(`Remove source ${app}? Already-ingested rows stay in the shards; only future polling stops.`)) {
      return
    }
    try {
      await deleteSource(namespace, app)
      onChanged()
    } catch (e) {
      alert(`Remove failed: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  async function loadSchema(app: string) {
    setSchemas(s => ({ ...s, [app]: null }))
    try {
      const sch = await getSourceSchema(namespace, app)
      setSchemas(s => ({ ...s, [app]: sch }))
    } catch (e) {
      alert(`Load schema failed: ${e instanceof Error ? e.message : String(e)}`)
      setSchemas(s => {
        const next = { ...s }
        delete next[app]
        return next
      })
    }
  }

  function toggleSchema(app: string) {
    if (app in schemas) {
      setSchemas(s => {
        const next = { ...s }
        delete next[app]
        return next
      })
      return
    }
    loadSchema(app)
  }

  async function handleResample(app: string) {
    if (!confirm(`Resample ${app}? Ingestion keeps running on the current schema; a fresh sample appears for review once the agent has collected enough lines.`)) {
      return
    }
    setBusy(b => ({ ...b, [app]: true }))
    try {
      await resampleSource(namespace, app)
      // Refresh an already-open schema panel so the "resample in
      // progress" state shows immediately.
      if (app in schemas) await loadSchema(app)
      onChanged()
    } catch (e) {
      alert(`Resample failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(b => ({ ...b, [app]: false }))
    }
  }

  return (
    <div
      className="csl-list"
      data-testid={`configured-sources-${namespace}-list`}
    >
      {sources.map(s => {
        const sch = schemas[s.app]
        const open = s.app in schemas
        return (
          <div
            key={s.app}
            className="csl-row"
            data-testid={`configured-source-row-${s.app}`}
          >
            <div className="csl-row-main">
              <span className="csl-app">{s.app}</span>
              <span className="csl-badge csl-badge-{s.source_type}">{s.source_type}</span>
              {s.source_type === 'cloudwatch' && (
                <span className="csl-meta">
                  {s.log_group} · {s.region} · every {s.poll_interval}
                </span>
              )}
              <span className="csl-meta csl-meta-dim">
                {s.last_poll_at
                  ? `last poll ${new Date(s.last_poll_at).toLocaleString()}`
                  : s.source_type === 'cloudwatch' ? 'never polled' : ''}
              </span>
            </div>
            <div className="csl-row-actions">
              <button
                className="psc-textbtn"
                onClick={() => toggleSchema(s.app)}
                data-testid={`configured-source-${s.app}-viewschema-button`}
              >
                {open ? 'Hide schema' : 'View schema'}
              </button>
              <button
                className="psc-textbtn"
                disabled={busy[s.app]}
                onClick={() => handleResample(s.app)}
                data-testid={`configured-source-${s.app}-resample-button`}
              >
                {busy[s.app] ? 'Resampling…' : 'Resample'}
              </button>
              <button
                className="psc-textbtn csl-remove"
                onClick={() => handleDelete(s.app)}
                data-testid={`configured-source-${s.app}-remove-button`}
              >
                Remove
              </button>
            </div>
            {open && (
              <div
                className="csl-schema-panel"
                data-testid={`configured-source-${s.app}-schema-panel`}
              >
                {sch === null && <span className="csl-meta csl-meta-dim">Loading…</span>}
                {sch && sch.status === 'configured' && (
                  <>
                    {sch.resample_requested && (
                      <div className="csl-meta csl-meta-dim">
                        Resample in progress: {sch.sample_collected ?? 0}/{sch.sample_target ?? 0} lines collected
                      </div>
                    )}
                    <pre className="csl-schema-yaml">{sch.schema_yaml}</pre>
                  </>
                )}
                {sch && sch.status === 'sampling' && (
                  <span className="csl-meta">
                    {s.app} hasn’t emitted enough lines to sample yet ({sch.sample_collected ?? 0}/{sch.sample_target ?? 0}).
                  </span>
                )}
                {sch && sch.status === 'pending' && (
                  <span className="csl-meta">
                    A fresh sample is awaiting configuration ({sch.sample_collected ?? 0}/{sch.sample_target ?? 0} lines).
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
