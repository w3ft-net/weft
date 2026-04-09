import { Link } from 'react-router-dom'
import TerminalDemo from '../components/TerminalDemo'
import PricingTable from '../components/PricingTable'
import './Landing.css'

export default function Landing() {
  return (
    <div className="landing">
      {/* Nav */}
      <nav className="nav" data-testid="nav">
        <div className="container nav-inner">
          <Link to="/" className="nav-logo" data-testid="nav-logo-link">
            <span className="logo-weft">weft</span>
          </Link>
          <div className="nav-links">
            <a href="#how-it-works" data-testid="nav-how-link">How it works</a>
            <a href="#pricing" data-testid="nav-pricing-link">Pricing</a>
            <Link to="/login" className="nav-cta" data-testid="nav-login-button">Log in</Link>
          </div>
        </div>
      </nav>

      {/* Hero */}
      <section className="hero">
        <div className="container">
          <div className="hero-content">
            <div className="hero-badge">grep · awk · sed · jq — on every log, everywhere</div>
            <h1 className="hero-title">
              <span className="hero-title-line">Unix-native</span>
              <span className="hero-title-line accent">log management</span>
            </h1>
            <p className="hero-sub">
              One shell. All your apps. Stream logs from EC2, Lambda, and containers
              into a single queryable pipeline. No vendor lock-in — your data exports
              to Parquet.
            </p>
            <div className="hero-actions">
              <Link to="/login" className="btn btn-primary" data-testid="hero-cta-button">
                Start free trial
              </Link>
              <a href="#how-it-works" className="btn btn-ghost" data-testid="hero-learn-link">
                See how it works →
              </a>
            </div>
          </div>
          <div className="hero-terminal">
            <TerminalDemo />
          </div>
        </div>
      </section>

      {/* How it works */}
      <section className="how-it-works" id="how-it-works">
        <div className="container">
          <h2 className="section-title">How it works</h2>
          <div className="steps">
            <div className="step">
              <div className="step-number">01</div>
              <div className="step-icon">
                <code>dpkg -i weftd.deb</code>
              </div>
              <h3>Install the agent</h3>
              <p>
                Drop <code>weftd</code> on your instances. It tails systemd journals
                and log files, streaming structured lines to the coordinator over gRPC.
              </p>
            </div>
            <div className="step-connector">
              <div className="connector-line" />
            </div>
            <div className="step">
              <div className="step-number">02</div>
              <div className="step-icon">
                <code>→ coordinator → shards</code>
              </div>
              <h3>Logs stream in</h3>
              <p>
                The coordinator ingests logs via a ring buffer, routes by app to
                time-sharded DuckDB nodes. Zero config — apps are discovered
                automatically.
              </p>
            </div>
            <div className="step-connector">
              <div className="connector-line" />
            </div>
            <div className="step">
              <div className="step-number">03</div>
              <div className="step-icon">
                <code>cat app.log | grep ERROR</code>
              </div>
              <h3>Query from anywhere</h3>
              <p>
                Use the <code>weft</code> CLI or the browser shell. Write pipelines
                with grep, awk, sed, jq — the tools you already know. Predicates
                push down to shard-level SQL.
              </p>
            </div>
          </div>
        </div>
      </section>

      {/* Pricing */}
      <section className="pricing" id="pricing">
        <div className="container">
          <h2 className="section-title">Simple, predictable pricing</h2>
          <p className="section-sub">
            14-day free trial on any tier. No credit card required.
            Usage-based overage — pay only for what you use beyond your plan.
          </p>
          <PricingTable />
        </div>
      </section>

      {/* Bottom CTA */}
      <section className="bottom-cta">
        <div className="container">
          <div className="cta-card">
            <h2>Stop copy-pasting SSH sessions.</h2>
            <p>Get all your logs in one shell — try weft free for 14 days.</p>
            <Link to="/login" className="btn btn-primary btn-lg" data-testid="bottom-cta-button">
              Start free trial
            </Link>
          </div>
        </div>
      </section>

      {/* Footer */}
      <footer className="footer">
        <div className="container footer-inner">
          <span className="footer-logo">weft</span>
          <span className="footer-copy">© 2026 Kevlar Consulting. All rights reserved.</span>
        </div>
      </footer>
    </div>
  )
}
