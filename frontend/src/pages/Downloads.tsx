import { useState } from 'react'
import SidebarNav from '../components/SidebarNav'
import Pane from '../components/Pane'
import './Downloads.css'

const RELEASES_BASE = 'https://releases.w3ft.net'

const CLI_LINKS = [
  { os: 'linux', arch: 'amd64', label: 'linux x86_64' },
  { os: 'linux', arch: 'arm64', label: 'linux arm64' },
  { os: 'darwin', arch: 'amd64', label: 'macos intel' },
  { os: 'darwin', arch: 'arm64', label: 'macos apple silicon' },
]

const AGENT_LINKS = [
  { arch: 'amd64', label: 'debian/ubuntu amd64' },
  { arch: 'arm64', label: 'debian/ubuntu arm64' },
]

function cliUrl(os: string, arch: string) {
  return `${RELEASES_BASE}/weft/latest/weft-${os}-${arch}`
}

// The release ext publishes the deb only under its version
// (weftd/v<ver>/weftd_<ver>_<arch>.deb); there is no "latest" deb.
function agentDebName(arch: string) {
  return `weftd_${__WEFTD_VERSION__}_${arch}.deb`
}

function agentUrl(arch: string) {
  return `${RELEASES_BASE}/weftd/v${__WEFTD_VERSION__}/${agentDebName(arch)}`
}

// CodeBlock renders shell commands as a terminal transcript: each
// command line gets a `$` prompt, comment-only blocks (config files) don't.
function CodeBlock({ code, testId, prompt = true }: { code: string; testId: string; prompt?: boolean }) {
  const [copied, setCopied] = useState(false)
  function handleCopy() {
    navigator.clipboard.writeText(code)
    setCopied(true)
    setTimeout(() => setCopied(false), 1200)
  }
  return (
    <div className="dl-code" data-testid={testId}>
      <pre className="dl-code-pre">
        {code.split('\n').map((line, i) => (
          <div key={i} className={line.trimStart().startsWith('#') ? 'dl-code-comment' : undefined}>
            {prompt && <span className="dl-code-prompt">$ </span>}
            {line}
          </div>
        ))}
      </pre>
      <button
        className="tui-btn dl-code-copy"
        onClick={handleCopy}
        data-testid={`${testId}-copy`}
      >
        {copied ? 'copied' : 'copy'}
      </button>
    </div>
  )
}

function FileList({ children }: { children: React.ReactNode }) {
  return (
    <div className="dl-files" role="list">
      <div className="dl-file-head" aria-hidden="true">
        <span>file</span>
        <span>platform</span>
      </div>
      {children}
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
            <h1 className="dash-page-title">downloads</h1>
          </div>

          {/* weft CLI */}
          <Pane title="weft" meta="cli" data-testid="downloads-cli-section">
            <p className="dl-section-sub">
              The interactive shell for querying logs. Runs on Linux and macOS.
            </p>

            <FileList>
              {CLI_LINKS.map(({ os, arch, label }) => (
                <a
                  key={`${os}-${arch}`}
                  href={cliUrl(os, arch)}
                  className="dl-file"
                  role="listitem"
                  data-testid={`downloads-cli-${os}-${arch}-link`}
                  download
                >
                  <span className="dl-file-name">weft-{os}-{arch}</span>
                  <span className="dl-file-label">{label}</span>
                </a>
              ))}
            </FileList>

            <div className="dl-install">
              <div className="dl-install-title">quick install, linux x86_64</div>
              <CodeBlock
                testId="downloads-cli-install-block"
                code={`curl -Lo weft ${cliUrl('linux', 'amd64')} && chmod +x weft && sudo mv weft /usr/local/bin/`}
              />
            </div>

            <div className="dl-install">
              <div className="dl-install-title">quick install, macos apple silicon</div>
              <CodeBlock
                testId="downloads-cli-macos-block"
                code={`curl -Lo weft ${cliUrl('darwin', 'arm64')} && chmod +x weft && sudo mv weft /usr/local/bin/`}
              />
            </div>
          </Pane>

          {/* weftd agent */}
          <Pane title="weftd" meta="agent" data-testid="downloads-agent-section">
            <p className="dl-section-sub">
              Deploy on EC2 instances and containers to stream logs in real time via gRPC.
            </p>

            <FileList>
              {AGENT_LINKS.map(({ arch, label }) => (
                <a
                  key={arch}
                  href={agentUrl(arch)}
                  className="dl-file"
                  role="listitem"
                  data-testid={`downloads-weftd-${arch}-link`}
                  download
                >
                  <span className="dl-file-name">{agentDebName(arch)}</span>
                  <span className="dl-file-label">{label}</span>
                </a>
              ))}
            </FileList>

            <div className="dl-install">
              <div className="dl-install-title"><span className="dl-step">1</span>install the package</div>
              <p className="dl-install-note">
                The package enables weftd on boot but does <strong>not</strong> start
                it — weftd refuses to run until you author its config (next step).
              </p>
              <CodeBlock
                testId="downloads-agent-install-block"
                code={`curl -LO ${agentUrl('amd64')}
sudo dpkg -i ${agentDebName('amd64')}`}
              />
            </div>

            <div className="dl-install">
              <div className="dl-install-title"><span className="dl-step">2</span>author the config</div>
              <p className="dl-install-note">
                Create a token on the{' '}
                <a href="/account" data-testid="downloads-account-link">account</a>{' '}
                page. weftd is configured entirely by{' '}
                <code>/etc/weftd/config.yaml</code> — the single source of truth,
                never generated or overwritten by the package. Copy{' '}
                <code>/etc/weftd/config.yaml.example</code> and edit:
              </p>
              <CodeBlock
                testId="downloads-agent-config-block"
                prompt={false}
                code={`# /etc/weftd/config.yaml
endpoint: w3ft.net:443
token: <your-agent-token>

# Optional host-wide dimensions stamped on every row
deployment: api
region: us-east-2
services: [api]

sources:
  # kind defaults to "main"; lands at /log/<ns>/myapp/main.log
  - service: myapp
    path: /var/log/myapp/*.log
    schema: json

  # tail a systemd unit's journal instead of a path
  - service: worker
    unit: worker.service`}
              />
              <p className="dl-install-note">
                The token may instead come from <code>WEFTD_TOKEN</code> in{' '}
                <code>/etc/default/weftd</code> (env wins over the file value) —
                useful when a secret injector supplies it out of band.
              </p>
            </div>

            <div className="dl-install">
              <div className="dl-install-title"><span className="dl-step">3</span>start and verify</div>
              <CodeBlock
                testId="downloads-agent-verify-block"
                code={`sudo systemctl start weftd
sudo systemctl status weftd
journalctl -u weftd -f`}
              />
            </div>
          </Pane>
        </div>
      </main>
    </div>
  )
}
