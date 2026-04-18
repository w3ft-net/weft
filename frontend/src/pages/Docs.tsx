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
              <li>
                Connect a source — either{' '}
                <a href="#install-weftd">install weftd</a> on a server you own, or{' '}
                <a href="#connect-cloudwatch">connect a CloudWatch log group</a> from your AWS account
              </li>
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

          {/* ── Connect a CloudWatch source ────────────── */}

          <section className="docs-section" id="connect-cloudwatch" data-testid="docs-connect-cloudwatch">
            <h2>Connect a CloudWatch source</h2>
            <p>
              If your logs already land in AWS CloudWatch Logs — Lambda, ECS/Fargate,
              API Gateway, RDS, or anything else that writes to a log group — weft can
              ingest directly without installing an agent. You grant a cross-account
              IAM role, point weft at a log group, and weft polls CloudWatch on a
              schedule (default 30s).
            </p>
            <p>
              This is the lowest-friction source type: nothing to install, nothing to
              run. Latency is bounded by CloudWatch's own ingestion delay plus the poll
              interval — usually well under a minute. For sub-second tailing you want{' '}
              <a href="#install-weftd">weftd</a> instead.
            </p>

            <h3>1. Create the cross-account IAM role</h3>
            <p>
              In the AWS account that owns the log group, create an IAM role that
              trusts weft's ingestion account and has read access to the log group.
              Pick an external ID — any hard-to-guess string — and keep it secret;
              you'll paste it into weft in step 2.
            </p>
            <pre className="docs-code">{`# Trust policy — allows weft's ingestion account to assume this role,
# but only when it presents the matching external ID.
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::220664822695:root" },
    "Action": "sts:AssumeRole",
    "Condition": {
      "StringEquals": { "sts:ExternalId": "REPLACE-WITH-YOUR-EXTERNAL-ID" }
    }
  }]
}

# Permissions policy — read-only access to the log group(s) you're sharing.
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "logs:FilterLogEvents",
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams"
    ],
    "Resource": "arn:aws:logs:us-east-2:123456789012:log-group:/aws/lambda/my-fn:*"
  }]
}`}</pre>
            <p>
              You can scope <code>Resource</code> to a single log group (recommended)
              or a prefix (<code>…/log-group:/aws/lambda/*</code>) to share a whole
              tier at once. Note the trailing <code>:*</code> — CloudWatch Logs ARNs
              require it.
            </p>

            <h3>2. Write a schema file</h3>
            <p>
              Weft needs to know how to parse each log line. The schema file is a
              short YAML document that names the app and tells weft where to find the
              timestamp. For a typical CloudWatch-flavored line
              (<code>2026-04-18 14:32:01 INFO {"{"}"user":"alice"{"}"}</code>):
            </p>
            <pre className="docs-code">{`# my-app.schema.yaml
app: my-app
format: text
timestamp_field: "1"          # 1-indexed field number
timestamp_fields: 2           # how many fields make up the timestamp
timestamp_format: "2006-01-02 15:04:05"   # Go time layout`}</pre>
            <p>
              If your lines are pure JSON (Lambda's structured logs, for example),
              use <code>format: json</code> and point <code>timestamp_field</code>{' '}
              at the JSON key instead:
            </p>
            <pre className="docs-code">{`app: my-lambda
format: json
timestamp_field: time        # the JSON key holding the timestamp
timestamp_format: "2006-01-02T15:04:05.000Z"`}</pre>

            <h3>3. Register the source</h3>
            <p>
              Use the <code>weft</code> CLI to tell weft about the account and log group.
              Run this from any machine with the <code>weft</code> binary installed and
              a valid login.
            </p>
            <pre className="docs-code">{`weft source add \\
  --app my-app \\
  --log-group /aws/lambda/my-fn \\
  --account 123456789012 \\
  --role-arn arn:aws:iam::123456789012:role/weft-ingest \\
  --external-id REPLACE-WITH-YOUR-EXTERNAL-ID \\
  --region us-east-2 \\
  --interval 30s \\
  --schema ./my-app.schema.yaml`}</pre>
            <p>
              Within one poll interval weft will assume the role, page through recent
              log events, and start writing rows. Confirm the source is live:
            </p>
            <pre className="docs-code">{`weft source list

# And in the shell:
cat /log/my-app.log | head`}</pre>

            <h3>Troubleshooting</h3>
            <ul>
              <li>
                <strong>No rows appearing</strong> — check the coordinator log for{' '}
                <code>poller[my-app]: FilterLogEvents: …</code> messages. The most
                common causes are a missing external ID, the role's resource ARN not
                matching your log group, or the log group being in a different region
                than you specified.
              </li>
              <li>
                <strong>Rows appearing but fields look wrong</strong> — your schema's{' '}
                <code>timestamp_format</code> is off. Check against Go's reference
                time (<code>Mon Jan 2 15:04:05 MST 2006</code>).
              </li>
              <li>
                <strong>CloudWatch charges</strong> — weft's poller uses
                <code>FilterLogEvents</code>, which AWS bills as data scanned. If cost
                matters, raise <code>--interval</code> (fewer calls) or scope your
                log group tightly.
              </li>
            </ul>
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

# Watch file format: a single '# schedule:' directive plus a shell
# pipeline. Alerts are dispatched by the 'alert' stage (see below);
# there's no watch-level notify field.
# schedule: */5 * * * *
cat myapp.log --since 5m | awk '$LEVEL == "ERROR"' | wc -l | alert above 0 --notify=ops`}</pre>
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
              Alerts are dispatched by the <code>alert</code> built-in — a pipeline stage you
              add to a watch (or run ad-hoc in the shell). It takes an optional condition and a{' '}
              <code>--notify=&lt;alias&gt;</code> flag pointing at an alias in{' '}
              <code>/etc/notify.conf</code>. One edit to an alias re-routes every watch that uses it.
            </p>

            <h3>1. Connect your integrations</h3>
            <p>
              Credentials never live in <code>notify.conf</code>. Connect them under{' '}
              <Link to="/account" data-testid="docs-account-link">Account → Integrations</Link>:
            </p>
            <ul>
              <li><strong>Slack</strong> — OAuth. weft posts via <code>chat.postMessage</code> to any channel the bot is a member of.</li>
              <li><strong>PagerDuty</strong> — for each PD service you want to page, paste its Events v2 integration key and give it a short name (e.g. <code>ops-daytime</code>). Aliases reference these names.</li>
            </ul>

            <h3>2. Edit <code>/etc/notify.conf</code></h3>
            <p>
              From the shell, <code>vi /etc/notify.conf</code>. The file is a YAML map of alias
              names to per-service targets:
            </p>
            <pre className="docs-code">{`# /etc/notify.conf

ops:
  slack:
    - "#ops-alerts"
    - "#backend"
  pagerduty:
    - name: ops-daytime
      severity: error

critical:
  slack:
    - "#pager"
  pagerduty:
    - name: oncall
      severity: critical

quiet:
  slack:
    - "#debug-noise"`}</pre>
            <p>
              Each service entry accepts either a bare string or an object. Bare strings are fine
              for Slack (it's just a channel); PagerDuty entries usually want <code>severity</code>{' '}
              to drive your escalation policy.
            </p>
            <ul>
              <li><code>slack</code> — list of channel names. Weft posts one message per listed channel.</li>
              <li>
                <code>pagerduty</code> — list of PD integrations. Each needs a <code>name</code>{' '}
                matching one you added under Account → Integrations. Optional{' '}
                <code>severity</code>: <code>critical</code>, <code>error</code> (default),{' '}
                <code>warning</code>, or <code>info</code>.
              </li>
            </ul>
            <p>
              Saves are validated — an invalid <code>notify.conf</code> (unknown severity, empty
              alias, malformed YAML) is rejected at write time so you don't find out three days
              later when a watch fires.
            </p>

            <h3>3. Pipe through <code>alert</code> in your watch</h3>
            <pre className="docs-code">{`# /cron.d/error-rate

# schedule: */5 * * * *
cat myapp.log --since 5m \\
  | awk '$LEVEL == "ERROR"' \\
  | wc -l \\
  | alert above 0 --notify=ops`}</pre>
            <p>
              <code>alert</code> reads stdin, evaluates its condition, and — when triggered and{' '}
              <code>--notify</code> is set — fans out to every Slack channel and PagerDuty
              integration in the alias.
            </p>
            <p>
              If you omit the condition, <code>alert</code> fires on any non-empty stdin. Conditions
              available: <code>above</code>, <code>below</code>, <code>eq</code>, <code>neq</code>,{' '}
              <code>changed</code>, <code>absent</code>, <code>rising</code>, <code>falling</code>.{' '}
              Run <code>alert --help</code> in the shell for the full reference.
            </p>

            <h3>4. Test it from the shell</h3>
            <p>
              Because <code>alert</code> is a normal built-in, you can fire a real dispatch at any
              time to verify your integrations and routing:
            </p>
            <pre className="docs-code">{`# Fires the 'ops' alias right now
echo "integration test — please ignore" | alert --notify=ops

# Same, with a condition
echo 99 | alert above 50 --notify=critical`}</pre>

            <h3>How PagerDuty alerts behave</h3>
            <p>
              Each PD event uses a <code>dedup_key</code> derived from the watch name, so a watch
              that fires every 5 minutes during an outage collapses to a single incident rather
              than a page storm. Ad-hoc shell invocations use a fresh synthetic name per run, so
              every test creates its own incident. The <code>severity</code> you set on the alias
              drives PD's routing rules. The incident summary includes the watch name and the
              first line of stderr; full stderr (capped at 1&nbsp;KB) is attached under custom
              details.
            </p>
          </section>
        </div>
      </main>
    </div>
  )
}
