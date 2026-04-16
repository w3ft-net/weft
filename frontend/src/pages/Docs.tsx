import { useEffect } from 'react'
import { useLocation, Link } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import './Docs.css'

export default function Docs() {
  const { hash } = useLocation()

  useEffect(() => {
    if (hash) {
      const el = document.getElementById(hash.slice(1))
      if (el) el.scrollIntoView({ behavior: 'smooth' })
    }
  }, [hash])

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="docs-content" data-testid="docs-main">

          <h1 className="docs-page-title">Documentation</h1>

          {/* ── Getting Started ─────────────────────────── */}

          <section className="docs-section docs-getting-started" data-testid="docs-getting-started">
            <h2>Getting Started</h2>
            <p>
              Welcome to weft. Follow these steps to go from zero to querying your logs:
            </p>
            <ol className="docs-steps">
              <li><a href="#create-namespace">Create a namespace</a> to group your log sources</li>
              <li><a href="#create-token">Create a token</a> so your agents can authenticate</li>
              <li><a href="#install-weftd">Install weftd</a> on a server and configure your log sources</li>
              <li><a href="#schema-setup">Check schema setup</a> to verify weft detected your log format</li>
              <li><a href="#query-logs">Query your logs</a> using the weft shell</li>
              <li><a href="#monitors">Set up a monitor</a> to watch for patterns</li>
              <li><a href="#alerting">Configure alerting</a> to get notified when monitors fire</li>
            </ol>
          </section>

          {/* ── Create a Namespace ─────────────────────── */}

          <section className="docs-section" id="create-namespace" data-testid="docs-create-namespace">
            <h2>Create a namespace</h2>
            <p>
              Namespaces group related log sources. A typical setup has one namespace per
              environment (e.g., <code>production</code>, <code>staging</code>) or per team.
            </p>
            <p>
              Go to <Link to="/namespaces" data-testid="docs-namespaces-link">Namespaces</Link> in
              the sidebar and click <strong>Create namespace</strong>. Choose a short, lowercase name
              — it becomes part of token identifiers and CLI scoping.
            </p>
            <pre className="docs-code">{`# You can also create namespaces from the shell:
namespace create production`}</pre>
          </section>

          {/* ── Create a Token ─────────────────────────── */}

          <section className="docs-section" id="create-token" data-testid="docs-create-token">
            <h2>Create a token</h2>
            <p>
              Tokens authenticate weftd agents. Each token is scoped to a single namespace.
              Go to <Link to="/namespaces" data-testid="docs-namespaces-tokens-link">Namespaces</Link>,
              select your namespace, and click <strong>Create token</strong>.
            </p>
            <p>
              Copy the token immediately — it's only shown once. It looks like{' '}
              <code>wft_a1b2c3d4e5f6...</code>
            </p>
            <pre className="docs-code">{`# Store the token in your weftd config:
token: wft_a1b2c3d4e5f6...`}</pre>
          </section>

          {/* ── Install weftd ──────────────────────────── */}

          <section className="docs-section" id="install-weftd" data-testid="docs-install-weftd">
            <h2>Install weftd</h2>
            <p>
              weftd is a lightweight Go binary that tails log files and ships structured lines to weft
              over gRPC. Download it from the <Link to="/downloads" data-testid="docs-downloads-link">Downloads</Link> page.
            </p>
            <pre className="docs-code">{`# Download and install
curl -fsSL https://w3ft.net/install.sh | sh

# Or on Debian/Ubuntu
sudo dpkg -i weftd_*.deb`}</pre>
            <p>Configure your log sources in <code>/etc/weftd/config.yaml</code>:</p>
            <pre className="docs-code">{`# /etc/weftd/config.yaml

endpoint: w3ft.net:443
token: wft_a1b2c3d4e5f6...

sources:
  - app: myapp
    path: /var/log/myapp/*.log
    schema: prefix_json
    prefix_fields: [ts, level, service]
    prefix_separator: " "
    json_field: payload

  - app: nginx
    path: /var/log/nginx/access.log
    schema: json`}</pre>
            <h3>Schema types</h3>
            <ul>
              <li><code>json</code> — pure JSON lines, no prefix</li>
              <li><code>prefix_json</code> — fixed-width prefix fields followed by a JSON payload</li>
              <li><code>raw</code> — unstructured text, no parsing</li>
            </ul>
            <pre className="docs-code">{`# Start weftd
sudo systemctl enable --now weftd

# Check status
sudo systemctl status weftd
journalctl -u weftd -f`}</pre>
          </section>

          {/* ── Schema Setup ───────────────────────────── */}

          <section className="docs-section" id="schema-setup" data-testid="docs-schema-setup">
            <h2>Check schema setup</h2>
            <p>
              Once weftd is shipping logs, weft auto-detects the schema from the first batch
              of lines. Go to <Link to="/namespaces" data-testid="docs-namespaces-schema-link">Namespaces</Link> to
              see detected apps and their field schemas.
            </p>
            <p>
              If you see a <strong>pending source</strong> notification on the dashboard, it means
              weft received data but needs you to confirm the detected schema. Review the sample
              lines and approve.
            </p>
            <p>
              Common fields weft extracts automatically:
            </p>
            <ul>
              <li><code>$TS</code> — timestamp</li>
              <li><code>$LEVEL</code> — log level (ERROR, WARN, INFO, DEBUG)</li>
              <li><code>$APP</code> — application name</li>
              <li><code>$INSTANCE</code> — hostname or instance ID</li>
            </ul>
          </section>

          {/* ── Query Logs ─────────────────────────────── */}

          <section className="docs-section" id="query-logs" data-testid="docs-query-logs">
            <h2>Query your logs</h2>
            <p>
              Open the <Link to="/shell" data-testid="docs-shell-link">Shell</Link> or use
              the <code>weft</code> CLI. Log sources appear as pseudofiles — use standard Unix
              pipeline idioms:
            </p>
            <pre className="docs-code">{`# Tail logs in real time
tail -f myapp.log

# Search for errors in the last hour
cat myapp.log | awk '$LEVEL == "ERROR"'

# Count requests by status code
cat nginx.log | jq '.status' | sort | uniq -c | sort -rn

# Grep across all sources
cat *.log | grep "connection refused"

# Time-bounded queries
cat myapp.log --since 2h --until 30m | head -100`}</pre>
            <p>
              The weft query planner analyzes your full pipeline and pushes predicates down into
              SQL — you write familiar shell commands, weft makes them fast.
            </p>
          </section>

          {/* ── Monitors ───────────────────────────────── */}

          <section className="docs-section" id="monitors" data-testid="docs-monitors">
            <h2>Set up a monitor</h2>
            <p>
              Monitors (called <strong>watches</strong> in weft) are saved pipelines that run on
              a schedule. Create one in the shell:
            </p>
            <pre className="docs-code">{`# Open the watch editor
vi /cron.d/error-count

# Watch file format:
# schedule: */5 * * * *
# notify: slack:#alerts
# ---
cat myapp.log --since 5m | awk '$LEVEL == "ERROR"' | wc -l`}</pre>
            <p>
              Watches run on the specified cron schedule. The pipeline's stdout is captured as the
              watch result — numeric values are graphed on the dashboard as sparklines.
            </p>
            <p>
              View and manage watches from the <Link to="/dashboard" data-testid="docs-dashboard-link">Dashboard</Link>.
            </p>
          </section>

          {/* ── Alerting ───────────────────────────────── */}

          <section className="docs-section" id="alerting" data-testid="docs-alerting">
            <h2>Configure alerting</h2>
            <p>
              Watches can send notifications when they produce output. Set the <code>notify</code> destination
              in the watch file:
            </p>
            <pre className="docs-code">{`# Slack channel
notify: slack:#ops-alerts

# PagerDuty service
notify: pagerduty:my-service

# Multiple destinations
notify: slack:#alerts,pagerduty:critical`}</pre>
            <p>
              Connect your integrations in{' '}
              <Link to="/account" data-testid="docs-account-link">Account</Link> settings:
            </p>
            <ul>
              <li><strong>Slack</strong> — OAuth connection, select which channels to post to</li>
              <li><strong>PagerDuty</strong> — add your integration key per service</li>
            </ul>
            <p>
              Notification routing can be configured to send different watches to different channels
              based on pattern matching.
            </p>
          </section>
        </div>
      </main>
    </div>
  )
}
