import { Link } from 'react-router-dom'
import { PLANS, DEMO_URL } from '../lib/plans'
import './PricingTable.css'

export default function PricingTable() {
  return (
    <div className="pricing-grid" data-testid="pricing-table">
      {PLANS.map(tier => (
        <div key={tier.name} className={`pricing-card ${tier.accent ? 'featured' : ''}`}>
          <div className="pricing-head">
            <h3 className="pricing-name">{tier.name.toLowerCase()}</h3>
            {tier.accent && <span className="pricing-badge">most popular</span>}
          </div>
          <div className="pricing-price">
            {tier.price === null ? (
              <span className="pricing-amount">custom</span>
            ) : (
              <>
                <span className="pricing-dollar">$</span>
                <span className="pricing-amount">{tier.price}</span>
                <span className="pricing-period">/mo</span>
              </>
            )}
          </div>
          {tier.blurb ? (
            <p className="pricing-blurb">{tier.blurb}</p>
          ) : (
            <>
              <div className="pricing-meta">
                <div className="pricing-meta-item">
                  <span className="pricing-meta-label">retention</span>
                  <span className="pricing-meta-value">{tier.retention}</span>
                </div>
                <div className="pricing-meta-item">
                  <span className="pricing-meta-label">volume</span>
                  <span className="pricing-meta-value">{tier.volume}</span>
                </div>
                <div className="pricing-meta-item">
                  <span className="pricing-meta-label">overage</span>
                  <span className="pricing-meta-value">{tier.overage}</span>
                </div>
              </div>
              <ul className="pricing-features">
                {tier.features?.map(f => (
                  <li key={f}>
                    <span className="pricing-check">+</span>
                    {f}
                  </li>
                ))}
              </ul>
            </>
          )}
          {tier.price === null ? (
            <a href={DEMO_URL} target="_blank" rel="noopener noreferrer" className="pricing-cta" data-testid={`pricing-${tier.tier}-button`}>
              talk to us
            </a>
          ) : (
            <Link to={`/signup?tier=${tier.tier}`} className="pricing-cta" data-testid={`pricing-${tier.tier}-button`}>
              start free trial
            </Link>
          )}
        </div>
      ))}
    </div>
  )
}
