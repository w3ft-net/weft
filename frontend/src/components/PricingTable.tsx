import { Link } from 'react-router-dom'
import './PricingTable.css'

const TIERS = [
  {
    name: 'Swatch',
    price: 19,
    retention: '30 days',
    volume: '10 GB/month',
    overage: '$2/GB',
    features: ['5 users', 'CLI + browser shell', '20 crons', '10MB tmp storage'],
    accent: false,
  },
  {
    name: 'Yard',
    price: 79,
    retention: '90 days',
    volume: '100 GB/month',
    overage: '$1/GB',
    features: ['10 users', 'CLI + browser shell', '100 crons', '100MB tmp storage'],
    accent: true,
  },
  {
    name: 'Bolt',
    price: 299,
    retention: '1 year',
    volume: '1 TB/month',
    overage: '$0.50/GB',
    features: ['50 users', 'CLI + browser shell', '500 crons', '1GB tmp storage'],
    accent: false,
  },
]

export default function PricingTable() {
  return (
    <div className="pricing-grid" data-testid="pricing-table">
      {TIERS.map(tier => (
        <div key={tier.name} className={`pricing-card ${tier.accent ? 'featured' : ''}`}>
          {tier.accent && <div className="pricing-badge">Most popular</div>}
          <h3 className="pricing-name">{tier.name}</h3>
          <div className="pricing-price">
            <span className="pricing-dollar">$</span>
            <span className="pricing-amount">{tier.price}</span>
            <span className="pricing-period">/mo</span>
          </div>
          <div className="pricing-meta">
            <div className="pricing-meta-item">
              <span className="pricing-meta-label">Retention</span>
              <span className="pricing-meta-value">{tier.retention}</span>
            </div>
            <div className="pricing-meta-item">
              <span className="pricing-meta-label">Volume</span>
              <span className="pricing-meta-value">{tier.volume}</span>
            </div>
            <div className="pricing-meta-item">
              <span className="pricing-meta-label">Overage</span>
              <span className="pricing-meta-value">{tier.overage}</span>
            </div>
          </div>
          <ul className="pricing-features">
            {tier.features.map(f => (
              <li key={f}>
                <span className="pricing-check">✓</span>
                {f}
              </li>
            ))}
          </ul>
          <Link to={`/signup?tier=${tier.name.toLowerCase()}`} className="pricing-cta" data-testid={`pricing-${tier.name.toLowerCase()}-button`}>
            Start free trial
          </Link>
        </div>
      ))}
    </div>
  )
}
