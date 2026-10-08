import { useState } from 'react'
import {
  configureSource,
  notifyPendingSourcesChanged,
  type PendingSource,
} from '../lib/api'
import SchemaEditor from './SchemaEditor'
import './PendingSourceConfigure.css'

export default function PendingSourceConfigure({
  source,
  onConfigured,
}: {
  source: PendingSource
  onConfigured: () => void
}) {
  const key = `${source.namespace}-${source.service}`
  const [expanded, setExpanded] = useState(false)

  // A still-sampling source has no full sample yet — it can't be
  // configured. Show progress instead of the schema editor so the user
  // knows it just needs to emit more lines.
  if (source.status === 'sampling') {
    return (
      <div className="psc-row" data-testid={`pending-source-row-${key}`}>
        <div className="psc-header psc-header-static" data-testid={`pending-source-sampling-${key}`}>
          <span className="psc-chevron" />
          <span className="psc-service-name">{source.service}</span>
          <span className="tui-tag tui-tag-warn">
            sampling {source.sample_collected}/{source.sample_target}
          </span>
          <span className="psc-meta">
            {source.service} hasn’t emitted enough lines to sample yet
          </span>
        </div>
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

      {expanded && (
        <SchemaEditor
          testidPrefix={`pending-source-${key}`}
          sample={source.sample ?? []}
          detection={source.detection}
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
