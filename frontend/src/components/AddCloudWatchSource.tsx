import { useState } from 'react'
import {
  sampleCloudWatchSource,
  addCloudWatchSource,
  type ConfigureSourceRequest,
  type SourceDetection,
} from '../lib/api'
import SchemaEditor from './SchemaEditor'
import './PendingSourceConfigure.css'

const POLL_INTERVALS = ['30s', '1m', '5m', '15m'] as const

export default function AddCloudWatchSource({
  namespace,
  onAdded,
  onCancel,
}: {
  namespace: string
  onAdded: () => void
  onCancel: () => void
}) {
  const testidPrefix = `add-cloudwatch-${namespace}`

  // Step 1: creds + log group identification.
  const [service, setService] = useState('')
  const [awsAccountId, setAwsAccountId] = useState('')
  const [roleArn, setRoleArn] = useState('')
  const [externalId, setExternalId] = useState('')
  const [logGroup, setLogGroup] = useState('')
  const [region, setRegion] = useState('us-east-2')
  const [pollInterval, setPollInterval] = useState<string>('30s')

  // Step 2: sample + detection, filled by the "Pull sample" action.
  const [sample, setSample] = useState<string[] | null>(null)
  const [detection, setDetection] = useState<SourceDetection | null>(null)

  const [pulling, setPulling] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const credsComplete =
    service.trim() !== '' &&
    logGroup.trim() !== '' &&
    roleArn.trim() !== '' &&
    region.trim() !== ''

  async function pullSample() {
    setPulling(true)
    setError(null)
    try {
      const resp = await sampleCloudWatchSource({
        namespace,
        aws_account_id: awsAccountId,
        role_arn: roleArn,
        external_id: externalId,
        log_group: logGroup,
        region,
      })
      if (!resp.lines || resp.lines.length === 0) {
        setError(
          'No events returned. Check that the log group name is correct and that the role has logs:FilterLogEvents on it. (Empty log groups also return nothing — try a noisier source to validate creds.)',
        )
        return
      }
      setSample(resp.lines)
      setDetection(resp.detection)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setPulling(false)
    }
  }

  async function submit(config: ConfigureSourceRequest) {
    await addCloudWatchSource({
      namespace,
      service: service.trim(),
      aws_account_id: awsAccountId.trim(),
      role_arn: roleArn.trim(),
      external_id: externalId.trim(),
      log_group: logGroup.trim(),
      region: region.trim(),
      poll_interval: pollInterval,
      ...config,
    })
    onAdded()
  }

  return (
    <div className="psc-row" data-testid={`${testidPrefix}-form`}>
      <div className="psc-body">
        <div className="psc-advanced">
          <label className="psc-field">
            <span className="psc-label">Service name</span>
            <input
              type="text"
              value={service}
              onChange={e => setService(e.target.value)}
              placeholder="e.g. webhooks"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-service-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">CloudWatch log group</span>
            <input
              type="text"
              value={logGroup}
              onChange={e => setLogGroup(e.target.value)}
              placeholder="/aws/lambda/my-function"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-log-group-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Region</span>
            <input
              type="text"
              value={region}
              onChange={e => setRegion(e.target.value)}
              placeholder="us-east-2"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-region-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">AWS account ID</span>
            <input
              type="text"
              value={awsAccountId}
              onChange={e => setAwsAccountId(e.target.value)}
              placeholder="12-digit AWS account number"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-aws-account-id-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Role ARN</span>
            <input
              type="text"
              value={roleArn}
              onChange={e => setRoleArn(e.target.value)}
              placeholder="arn:aws:iam::123456789012:role/weft-reader"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-role-arn-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">External ID (optional)</span>
            <input
              type="text"
              value={externalId}
              onChange={e => setExternalId(e.target.value)}
              placeholder="STS external ID secret"
              disabled={sample !== null}
              data-testid={`${testidPrefix}-external-id-input`}
            />
          </label>

          <label className="psc-field">
            <span className="psc-label">Poll interval</span>
            <select
              value={pollInterval}
              onChange={e => setPollInterval(e.target.value)}
              disabled={sample !== null}
              data-testid={`${testidPrefix}-poll-interval-select`}
            >
              {POLL_INTERVALS.map(v => (
                <option key={v} value={v}>{v}</option>
              ))}
            </select>
          </label>
        </div>

        {error && (
          <div className="psc-error" data-testid={`${testidPrefix}-error`}>
            {error}
          </div>
        )}

        {sample === null && (
          <div className="psc-actions">
            <button
              className="btn-action btn-action-primary"
              onClick={pullSample}
              disabled={!credsComplete || pulling}
              data-testid={`${testidPrefix}-pull-sample-button`}
            >
              {pulling ? 'Pulling sample…' : 'Pull sample'}
            </button>
            <button
              className="psc-textbtn"
              onClick={onCancel}
              data-testid={`${testidPrefix}-cancel-button`}
            >
              Cancel
            </button>
          </div>
        )}
      </div>

      {sample !== null && detection !== null && (
        <SchemaEditor
          testidPrefix={testidPrefix}
          sample={sample}
          detection={detection}
          submitLabel="Add CloudWatch source"
          submittingLabel="Adding…"
          onSubmit={submit}
        />
      )}
    </div>
  )
}
