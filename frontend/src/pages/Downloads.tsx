import SidebarNav from '../components/SidebarNav'
import './Downloads.css'

const RELEASES_BASE = 'https://releases.w3ft.net'

const CLI_LINKS = [
  { os: 'linux', arch: 'amd64', label: 'Linux x86_64' },
  { os: 'linux', arch: 'arm64', label: 'Linux arm64' },
  { os: 'darwin', arch: 'amd64', label: 'macOS Intel' },
  { os: 'darwin', arch: 'arm64', label: 'macOS Apple Silicon' },
]

const AGENT_LINKS = [
  { arch: 'amd64', label: 'amd64 .deb' },
  { arch: 'arm64', label: 'arm64 .deb' },
]

function cliUrl(os: string, arch: string) {
  return `${RELEASES_BASE}/weft/latest/weft-${os}-${arch}`
}

function agentUrl(arch: string) {
  return `${RELEASES_BASE}/weftd/latest/weftd_latest_${arch}.deb`
}

function CodeBlock({ code, testId }: { code: string; testId: string }) {
  function handleCopy() {
    navigator.clipboard.writeText(code)
  }
  return (
    <div className="code-block" data-testid={testId}>
      <pre className="code-block-pre">{code}</pre>
      <button
        className="code-block-copy"
        onClick={handleCopy}
        data-testid={`${testId}-copy`}
      >
        copy
      </button>
    </div>
  )
}

export default function Downloads() {
  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="downloads-main">
        <div className="downloads-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Downloads</h1>
          </div>

          {/* weft CLI */}
          <section className="dl-section" data-testid="downloads-cli-section">
            <div className="dl-section-header">
              <h2 className="dl-section-title">weft CLI</h2>
              <span className="dl-section-sub">
                The interactive shell for querying logs. Runs on Linux and macOS.
              </span>
            </div>

            <div className="dl-grid">
              {CLI_LINKS.map(({ os, arch, label }) => (
                <a
                  key={`${os}-${arch}`}
                  href={cliUrl(os, arch)}
                  className="dl-link"
                  data-testid={`downloads-cli-${os}-${arch}-link`}
                  download
                >
                  <span className="dl-link-icon">↓</span>
                  <div>
                    <div className="dl-link-label">{label}</div>
                    <div className="dl-link-meta">weft-{os}-{arch}</div>
                  </div>
                </a>
              ))}
            </div>

            <div className="dl-install">
              <div className="dl-install-title">Quick install (Linux x86_64)</div>
              <CodeBlock
                testId="downloads-cli-install-block"
                code={`curl -Lo weft ${cliUrl('linux', 'amd64')} && chmod +x weft && sudo mv weft /usr/local/bin/`}
              />
            </div>

            <div className="dl-install">
              <div className="dl-install-title">Quick install (macOS Apple Silicon)</div>
              <CodeBlock
                testId="downloads-cli-macos-block"
                code={`curl -Lo weft ${cliUrl('darwin', 'arm64')} && chmod +x weft && sudo mv weft /usr/local/bin/`}
              />
            </div>
          </section>

          {/* weftd agent */}
          <section className="dl-section" data-testid="downloads-agent-section">
            <div className="dl-section-header">
              <h2 className="dl-section-title">weftd agent</h2>
              <span className="dl-section-sub">
                Deploy on EC2 instances and containers to stream logs in real time via gRPC.
              </span>
            </div>

            <div className="dl-grid">
              {AGENT_LINKS.map(({ arch, label }) => (
                <a
                  key={arch}
                  href={agentUrl(arch)}
                  className="dl-link"
                  data-testid={`downloads-weftd-${arch}-link`}
                  download
                >
                  <span className="dl-link-icon">↓</span>
                  <div>
                    <div className="dl-link-label">{label}</div>
                    <div className="dl-link-meta">weftd_latest_{arch}.deb</div>
                  </div>
                </a>
              ))}
            </div>

            <div className="dl-install">
              <div className="dl-install-title">1. Install the package</div>
              <p className="dl-install-note">
                The package enables weftd on boot but does <strong>not</strong> start
                it — weftd refuses to run until you author its config (next step).
              </p>
              <CodeBlock
                testId="downloads-agent-install-block"
                code={`curl -LO ${agentUrl('amd64')}
sudo dpkg -i weftd_latest_amd64.deb`}
              />
            </div>

            <div className="dl-install">
              <div className="dl-install-title">2. Author the config</div>
              <p className="dl-install-note">
                Create a token on the{' '}
                <a href="/account" data-testid="downloads-account-link">Account</a>{' '}
                page. weftd is configured entirely by{' '}
                <code>/etc/weftd/config.yaml</code> — the single source of truth,
                never generated or overwritten by the package. Copy{' '}
                <code>/etc/weftd/config.yaml.example</code> and edit:
              </p>
              <CodeBlock
                testId="downloads-agent-config-block"
                code={`# /etc/weftd/config.yaml
endpoint: w3ft.net:443
token: <your-agent-token>

# Optional host-wide dimensions stamped on every row
deployment: api
region: us-east-2
services: [api]

sources:
  # kind defaults to "main"; lands at /log/<ns>/myapp/main.log
  - app: myapp
    path: /var/log/myapp/*.log
    schema: json

  # tail a systemd unit's journal instead of a path
  - app: worker
    unit: worker.service`}
              />
              <p className="dl-install-note">
                The token may instead come from <code>WEFTD_TOKEN</code> in{' '}
                <code>/etc/default/weftd</code> (env wins over the file value) —
                useful when a secret injector supplies it out of band.
              </p>
            </div>

            <div className="dl-install">
              <div className="dl-install-title">3. Start &amp; verify</div>
              <CodeBlock
                testId="downloads-agent-verify-block"
                code={`sudo systemctl start weftd
sudo systemctl status weftd
journalctl -u weftd -f`}
              />
            </div>
          </section>
        </div>
      </main>
    </div>
  )
}
