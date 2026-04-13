import { Link } from 'react-router-dom'
import AsciiLogo from '../components/AsciiLogo'
import VillainCarousel from '../components/VillainCarousel'
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
            <a href="#pricing" data-testid="nav-pricing-link">Pricing</a>
            <Link to="/login" className="nav-cta" data-testid="nav-login-button">Log in</Link>
          </div>
        </div>
      </nav>

      {/* ── Narrative: the problem ─────────────────────────────── */}

      <section className="hero">
        <div className="container">
          <div className="prison-row">
            <VillainCarousel side="left" startIndex={0} />
            <pre className="prison-box">{
`+---------------------------------------------------+
|                                                   |
|   Distributed computing has rent asunder your     |
|   log infrastructure. You are locked in a         |
|   vendor dungeon, your muscle memory for          |
|   familiar tools failing you every time you       |
|   must learn a new proprietary syntax.            |
|                                                   |
+---------------------------------------------------+`
            }</pre>
            <VillainCarousel side="right" startIndex={1} />
          </div>
        </div>

        <div className="spacer-lg" />

        {/* ── Narrative: the solution ────────────────────────────── */}

        <div className="narrative">
          <p>
            restore the fabric of Do One Thing Well with...
          </p>
        </div>

        <section className="hero-center container">
          <AsciiLogo />
        </section>

        <div className="narrative">
          <p>
            <em>weft</em> stitches together logs from disparate sources
            <br />and gives you cloud-native versions of core *nix utilities.
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
          <pre className="section-header">{
`+----------------------------------------------+
|         simple, predictable pricing          |
+----------------------------------------------+`
          }</pre>
          <p className="section-sub">
            14-day free trial on any tier. No credit card required.
            Usage-based overage — pay only for what you use beyond your plan.
          </p>
          <PricingTable />
        </div>
      </section>

      {/* Bottom CTA */}
      <section className="cta-section">
        <div className="container">
          <pre className="cta-box">{
`+------------------------------------------------------+
|                                                      |
|       Let pipes once again be the unifying           |
|       thread in you log analysis.                    |
|                                                      |
|       try weft free for 14 days.                     |
|                                                      |
+------------------------------------------------------+`
          }</pre>
          <div className="cta-center">
            <Link to="/login" className="cta-cmd" data-testid="bottom-cta-button">
              start free trial &rarr;
            </Link>
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
