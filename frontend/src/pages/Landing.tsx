import { Link } from 'react-router-dom'
import AsciiLogo from '../components/AsciiLogo'
import ScrollDiagram from '../components/ScrollDiagram'
import './Landing.css'

// Self-serve pricing/signup is hidden during private beta — the homepage funnels
// visitors to book a demo instead. Same booking link as graybeardcorp.com.
// Restore <PricingTable> + the /signup CTAs to bring self-serve back.
const DEMO_URL = 'https://calendar.app.google/6q35MfR67skswfsk7'

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
            <a href={DEMO_URL} target="_blank" rel="noopener noreferrer" data-testid="nav-demo-link">Schedule a demo</a>
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
            <a href={DEMO_URL} className="cta-cmd" data-testid="hero-cta-button" target="_blank" rel="noopener noreferrer">
              schedule a demo &rarr;
            </a>
          </div>

          <p style={{
            textAlign: 'center',
            fontFamily: 'var(--font-mono)',
            fontSize: 12,
            color: '#555577',
            marginTop: 8,
          }}>
            Private beta — book a live walkthrough with the team.
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

      {/* ── Schedule a demo ───────────────────────────────────── */}

      <section className="pricing-section" id="demo">
        <div className="container">
          <p className="section-sub">
            weft is in private beta. Book a live walkthrough and we'll get
            you set up — no credit card, no commitment.
          </p>
          <div className="cta-center" style={{ marginTop: 24 }}>
            <a href={DEMO_URL} className="cta-cmd" data-testid="demo-cta-button" target="_blank" rel="noopener noreferrer">
              schedule a demo &rarr;
            </a>
          </div>
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
