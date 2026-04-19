import { deleteSource, type SourceInfo } from '../lib/api'
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

  return (
    <div
      className="csl-list"
      data-testid={`configured-sources-${namespace}-list`}
    >
      {sources.map(s => (
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
          <button
            className="psc-textbtn csl-remove"
            onClick={() => handleDelete(s.app)}
            data-testid={`configured-source-${s.app}-remove-button`}
          >
            Remove
          </button>
        </div>
      ))}
    </div>
  )
}
