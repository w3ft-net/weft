import { Link } from 'react-router-dom'
import AsciiLogo from '../components/AsciiLogo'
import ScrollDiagram from '../components/ScrollDiagram'
import PricingTable from '../components/PricingTable'
import './Landing.css'

export default function Landing() {
  return (
    <div className="landing">
      {/* Nav */}
      <nav className="nav" data-testid="nav">
        <div className="container nav-inner">
          <Link to="/" className="nav-logo" data-testid="nav-logo-link">
            <img src="/weft-logo.svg" alt="weft" className="logo-nav" data-testid="weft-logo-svg" />
          </Link>
          <div className="nav-links">
            <a href="#compare" data-testid="nav-compare-link">Why weft</a>
            <a href="#pricing" data-testid="nav-pricing-link">Pricing</a>
            <Link to="/login" className="nav-cta" data-testid="nav-login-button">Log in</Link>
          </div>
        </div>
      </nav>

      {/* ── Hero: logo + tagline ──────────────────────────────── */}

      <section className="hero">
        <div className="container">

          <div className="narrative" style={{ paddingTop: 32 }}>
            <p>
              <strong>Hey you! Why be shackled to the browser with another proprietary DSL when you can use...</strong>
            </p>
          </div>
          <section className="hero-center">
            <AsciiLogo />
          </section>
          <div className="narrative" style={{ paddingTop: 32 }}>
            <p>
              <strong>A log aggregation platform that loves the terminal as much as you do. A balm for your Unix soul.</strong>
            </p>
          </div>

          <div className="cta-center" style={{ marginTop: 28, marginBottom: 16 }}>
            <Link to="/signup" className="cta-cmd" data-testid="hero-cta-button">
              start free trial &rarr;
            </Link>
          </div>

          <p style={{
            textAlign: 'center',
            fontFamily: 'var(--font-mono)',
            fontSize: 12,
            color: '#555577',
            marginTop: 8,
          }}>
            14 days free. No credit card required.
          </p>
        </div>
        <div className="narrative">
          <p>
            <em>weft</em> gives you cloud-native versions of core *nix utilities so that you can search, monitor, and alert on all your logs using sed, awk, jq, and more.
          </p>
        </div>
      </section>

      {/* ── Scroll-driven diagram → shell ─────────────────────── */}

      <div className="container">
        <ScrollDiagram />
      </div>

      {/* ── Pricing ───────────────────────────────────────────── */}

      <section className="pricing-section" id="pricing">
        <div className="container">
          <p className="section-sub">
            14-day free trial on any tier.
            Usage-based overage — pay only for what you use beyond your plan.
          </p>
          <PricingTable />
        </div>
      </section>

      {/* Footer */}
      <footer className="footer">
        <div className="container">
          weft &copy; 2026 <a href="https://graybeardcorp.com" data-testid="footer-company-link">Graybeard Corporation</a>
        </div>
      </footer>
    </div>
  )
}
