// The posted plans, in one place so the landing page, signup, payment step and
// account page cannot drift apart. Prices and entitlements follow
// scripts/costmodel/tiers.yaml; the code-side limits live in store/user.go.
//
// Bolt has no public price or posted limits: retention, volume and support are
// shaped per customer, so its price is null, its card shows a blurb in place of
// the numbers, and the UI sends visitors to the booking link instead of into
// checkout.

export const DEMO_URL = 'https://calendar.app.google/6q35MfR67skswfsk7'

export interface Plan {
  tier: 'swatch' | 'yard' | 'bolt'
  name: string
  price: number | null
  // Posted limits; absent on a plan that is quoted per customer.
  retention?: string
  volume?: string
  overage?: string
  features?: string[]
  // Shown instead of the limits when there are none to post.
  blurb?: string
  accent: boolean
}

export const PLANS: Plan[] = [
  {
    tier: 'swatch',
    name: 'Swatch',
    price: 29,
    retention: '30 days',
    volume: '10 GB/month',
    overage: '$2/GB',
    features: ['5 users', 'CLI + browser shell', '20 crons', '10MB tmp storage'],
    accent: false,
  },
  {
    tier: 'yard',
    name: 'Yard',
    price: 99,
    retention: '90 days',
    volume: '100 GB/month',
    overage: '$1/GB',
    features: ['10 users', 'CLI + browser shell', '100 crons', '100MB tmp storage'],
    accent: true,
  },
  {
    tier: 'bolt',
    name: 'Bolt',
    price: null,
    blurb: 'Need higher throughput, custom retention, or technical support? We\'d be happy to help.',
    accent: false,
  },
]

// Plans a visitor can start on their own: the ones with a posted price.
export const SELF_SERVE_PLANS = PLANS.filter(p => p.price !== null)

// "swatch  $29/mo" — the line the signup and payment steps show.
export const TIER_LABELS: Record<string, string> = Object.fromEntries(
  SELF_SERVE_PLANS.map(p => [p.tier, `${p.tier}  $${p.price}/mo`]),
)
