import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const now = Date.now()
const iso = (msAgo: number) => new Date(now - msAgo).toISOString()
function series(name: string, n = 48, base = 50, amp = 30, tag = '') {
  return Array.from({ length: n }, (_, i) => {
    const v = Math.max(0, Math.round(base + amp * Math.sin(i / 5 + name.length) + (Math.random() - 0.5) * amp * 0.6))
    return { ID: i, CronWatchID: 1, RanAt: iso((n - i) * 300000), Stdout: String(v), Stderr: '', ExitCode: 0, DurationMs: 120 + i, Value: v, Tag: tag }
  }).reverse() // API returns newest first
}
function watch(name: string, pipeline: string, extra: any = {}) {
  return { ID: name.length, Name: name, Schedule: '*/5 * * * *', Pipeline: pipeline, Enabled: true, LastRunAt: iso(60000), LastStatus: 'ok', CreatedAt: iso(9e8), UpdatedAt: iso(9e7), AlertCondition: null, AlertThreshold: null, ...extra }
}
const watches: Record<string, any> = {
  'errors-5m': { watch: watch('errors-5m', "cat /log/prod/web.log | grep -c ' 5[0-9][0-9] '", { AlertCondition: 'above', AlertThreshold: 80 }), results: series('errors-5m', 48, 40, 35) },
  'p95-latency': { watch: watch('p95-latency', 'cat /log/prod/web.log | awk \'{print $NF}\' | p95'), results: [...series('a', 48, 220, 60, 'web'), ...series('bbbb', 48, 140, 40, 'api')] },
  'ingest-rate': { watch: watch('ingest-rate', 'cat /log/**/*.log | wc -l'), results: series('ingest', 48, 12000, 3000) },
  'disk-used': { watch: watch('disk-used', 'cat /log/prod/node.log | jq .disk_pct | tail -1'), results: series('disk', 48, 71, 4) },
  'top-paths': { watch: watch('top-paths', 'cat /log/prod/web.log | awk \'{print $7}\' | sort | uniq -c | sort -rn | head'), results: [{ ...series('t', 1)[0], Stdout: '  4120 /api/v1/orders\n  2981 /api/v1/cart\n  1877 /health\n   902 /api/v1/login\n   410 /static/app.js' }] },
}
const user = { id: 'u1', email: 'paul@graybeardcorp.com', name: 'Paul', tier: 'team', customer: 'kevlar', orgs: [{ id: 'o1', name: 'kevlar', role: 'admin', active: true }, { id: 'o2', name: 'loom', role: 'member', active: false }], email_verified: true, has_seen_welcome: true, onboarding_step: 'done', onboarding_dismissed: true, is_superadmin: true }
const routes: [RegExp, any][] = [
  [/^\/api\/auth\/me/, user],
  [/^\/api\/dashboards\/[^/?]+/, { name: 'prod', updated_at: iso(3600000), spec: { title: 'production overview', sections: [
    { title: 'traffic', tiles: [{ kind: 'counter', watch: 'ingest-rate', format: '%d lines/5m' }, { kind: 'sparkline', watch: 'errors-5m', size: 'md', threshold: { warn: 60, crit: 80 } }, { kind: 'gauge', watch: 'disk-used', threshold: { warn: 75, crit: 90, max: 100 }, format: '%' }] },
    { title: 'latency', tiles: [{ kind: 'sparkline', watch: 'p95-latency', size: 'lg' }, { kind: 'table', watch: 'top-paths', columns: ['count', 'path'] }] },
  ] }, watches }],
  [/^\/api\/dashboards/, { dashboards: [{ name: 'prod', title: 'production overview', updated_at: iso(3600000) }, { name: 'billing', title: 'billing pipeline' }] }],
  [/^\/api\/watches\/[^/]+\/results/, series('errors-5m')],
  [/^\/api\/watches\/[^/]+\/chart/, { granularity: 'raw', range: '24h', points: [...series('x', 96, 40, 30, ''), ...series('yyyy', 96, 25, 15, 'eu')].map(r => ({ ts: new Date(r.RanAt).getTime(), tag: r.Tag, value: r.Value, count: 1, min: r.Value, max: r.Value })) }],
  [/^\/api\/watches\/[^/?]+/, watches['errors-5m'].watch],
  [/^\/api\/watches/, { watches: Object.values(watches).map((w: any) => w.watch), crons: Object.values(watches).map((w: any) => w.watch) }],
  [/^\/api\/usage/, { tier: 'team', bytes_ingested: 38.2e9, tier_limit_bytes: 100e9, period_start: iso(12 * 864e5), projected_overage_cents: 0, metrics_forecast: { agents_with_metrics: 4, projected_bytes_month: 61e9 } }],
  [/^\/api\/team/, { members: [{ ID: 'm1', CustomerName: 'kevlar', Email: 'paul@graybeardcorp.com', UserID: 'u1', Role: 'admin', Status: 'active', InvitedAt: iso(9e9), AcceptedAt: iso(9e9) }, { ID: 'm2', CustomerName: 'kevlar', Email: 'ops@graybeardcorp.com', UserID: null, Role: 'member', Status: 'pending', InvitedAt: iso(9e7), AcceptedAt: null }], seats_used: 2, seats_limit: 5, org_name: 'kevlar' }],
  [/^\/api\/integrations/, { slack: { connected: true, team_name: 'graybeard', configured_at: iso(9e8) }, pagerduty: { connected: false, keys: {}, integrations: [] } }],
  [/^\/api\/namespaces/, { namespaces: [{ name: 'prod', id: 1 }, { name: 'staging', id: 2 }] }],
  [/^\/api\/permissions/, {}],
  [/^\/api\/tokens/, [{ id: 1, prefix: 'wft_a1b2', label: 'web-asg', namespace: 'prod', env: 'prod', status: 'active', created_at: iso(9e8) }, { id: 2, prefix: 'wft_c3d4', label: 'old-box', namespace: 'staging', env: 'staging', status: 'revoked', created_at: iso(9e9) }]],
  [/^\/api\/sources\/[^/]+\/[^/]+\/schema/, { status: 'configured', schema_hash: 'abc', resample_requested: true, sample_collected: 80, sample_target: 200, schema_yaml: 'format: text\ntimestamp:\n  field: 0\n  format: 2006-01-02T15:04:05Z07:00\nlevel:\n  field: 1\n' }],
  [/^\/api\/sources\/pending/, [{ service: 'mailer', namespace: 'prod', instance: 'i-0def', host_group: 'workers', sample: [], detection: {}, created_at: iso(1e6), status: 'sampling', sample_collected: 120, sample_target: 200 }, { service: 'worker', namespace: 'prod', instance: 'i-0abc', host_group: 'workers', sample: ['2026-10-06T12:00:01Z INFO job=send_email id=42 took=120ms', '2026-10-06T12:00:02Z WARN job=resize id=43 retry=1'], detection: { JSONCoverage: 0, JSONTailCoverage: 0, TimestampCandidates: [{ FieldIndex: 0, SpanFields: 1, Format: '2006-01-02T15:04:05Z07:00', JSONPath: '', Sample: '2026-10-06T12:00:01Z' }] }, created_at: iso(3e6), status: 'pending', sample_collected: 200, sample_target: 200 }]],
  [/^\/api\/sources/, [{ namespace: 'prod', service: 'web', source_type: 'weftd', log_group: '', region: 'us-east-2', enabled: true }, { namespace: 'prod', service: 'api', source_type: 'kinesis', log_group: '/aws/ecs/api', region: 'us-east-2', enabled: true }]],
  [/^\/api\/syslog-producers/, [{ name: 'render', label: 'Render' }, { name: 'heroku', label: 'Heroku' }]],
  [/^\/api\/functions/, [{ ID: 1, Name: 'p95', Source: "sort -n | awk '{a[NR]=$1} END{print a[int(NR*.95)]}'", CreatedAt: iso(9e8), UpdatedAt: iso(9e8) }]],
  [/^\/api\/fs\/list/, { entries: [{ name: 'prod', is_dir: true }, { name: 'staging', is_dir: true }, { name: 'web.log', is_dir: false }, { name: 'api.log', is_dir: false }] }],
  [/^\/api\/ingestion-status/, { affected_services: [] }],
  [/^\/api\/admin\/customers/, [{ id: 'o1', name: 'kevlar', display_name: 'Kevlar', tier: 'team', is_comp: true, comp_reason: 'internal', status: 'active', created_at: iso(9e9) }, { id: 'o3', name: 'acme', display_name: 'Acme', tier: 'starter', is_comp: false, status: 'active', created_at: iso(9e8) }]],
  [/^\/api\/admin\/buckets\/\d+\/shards/, []],
  [/^\/api\/admin\/buckets/, [{ ID: 1, Services: [], WindowDuration: '24h', TargetIngestingCount: 2, Tier: 'standard', CreatedAt: iso(9e9) }]],
]
export default defineConfig({
  root: process.env.WEFT_FE,
  plugins: [react(), {
    name: 'weft-mock-api',
    transformIndexHtml(html: string) {
      return html.replace('<head>', `<head><script>try{localStorage.setItem('weft_token','mock')}catch(e){}</script>`)
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (!req.url?.startsWith('/api/')) return next()
        const hit = routes.find(([re]) => re.test(req.url!))
        res.setHeader('Content-Type', 'application/json')
        res.end(JSON.stringify(hit ? hit[1] : req.method === 'GET' ? [] : {}))
      })
    },
  }],
  define: { __WEFT_WEB_SHA__: JSON.stringify('mock') },
})
