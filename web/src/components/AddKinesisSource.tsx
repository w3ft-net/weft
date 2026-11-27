import { useState } from 'react'
import { addKinesisSource } from '../lib/api'
import './PendingSourceConfigure.css'

// AddKinesisSource registers a CloudWatch-push source: no role/account
// fields (Firehose pushes to us, nothing to assume a role into) and no
// "pull sample" step (there's nothing to poll yet — the sample
// accumulates from the customer's own Firehose deliveries once their CFN
// stack is live, and the resulting source shows up under "Pending
// sources" to finish through <PendingSourceConfigure> like any other
// pending source).
export default function AddKinesisSource({
  namespace,
  onAdded,
  onCancel,
}: {
  namespace: string
  onAdded: () => void
  onCancel: () => void
}) {
  const testidPrefix = `add-kinesis-${namespace}`

  const [service, setService] = useState('')
  const [logGroup, setLogGroup] = useState('')
  const [region, setRegion] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const complete =
    service.trim() !== '' && logGroup.trim() !== '' && region.trim() !== ''

  async function submit() {
    setSubmitting(true)
    setError(null)
    try {
      await addKinesisSource({
        namespace,
        service: service.trim(),
        locator: logGroup.trim(),
        params: { region: region.trim() },
      })
      onAdded()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="psc-row psc-row-form" data-testid={`${testidPrefix}-form`}>
      <div className="psc-body">
        <div className="psc-form-title">add kinesis source</div>
        <div className="psc-advanced psc-advanced-stack">
          <label className="psc-field">
            <span className="psc-label">service:</span>
            <input
              type="text"
              value={service}
              onChange={e => setService(e.target.value)}
              placeholder="e.g. webhooks"
              data-testid={`${testidPrefix}-service-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">log group:</span>
            <input
              type="text"
              value={logGroup}
              onChange={e => setLogGroup(e.target.value)}
              placeholder="/aws/lambda/my-function"
              data-testid={`${testidPrefix}-log-group-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">region:</span>
            <input
              type="text"
              value={region}
              onChange={e => setRegion(e.target.value)}
              placeholder="us-east-2"
              data-testid={`${testidPrefix}-region-input`}
            />
          </label>
        </div>

        <p className="psc-hint">
          after adding, deploy the Firehose CloudFormation stack against
          this log group. once events arrive, the source shows up under
          “pending sources” to finish configuring.
        </p>

        {error && (
          <div className="psc-error" data-testid={`${testidPrefix}-error`}>
            error: {error}
          </div>
        )}

        <div className="psc-actions">
          <button
            className="tui-btn tui-btn-primary"
            onClick={submit}
            disabled={!complete || submitting}
            data-testid={`${testidPrefix}-submit-button`}
          >
            {submitting ? 'adding…' : 'add source'}
          </button>
          <button
            className="tui-btn"
            onClick={onCancel}
            data-testid={`${testidPrefix}-cancel-button`}
          >
            cancel
          </button>
        </div>
      </div>
    </div>
  )
}
