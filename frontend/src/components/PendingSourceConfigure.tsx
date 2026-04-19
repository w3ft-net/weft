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
  const key = `${source.namespace}-${source.app}`
  const [expanded, setExpanded] = useState(false)

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
        <SchemaEditor
          testidPrefix={`pending-source-${key}`}
          sample={source.sample ?? []}
          detection={source.detection}
          onSubmit={async (config) => {
            await configureSource(source.namespace, source.app, config)
            notifyPendingSourcesChanged()
            onConfigured()
          }}
        />
      )}
    </div>
  )
}
