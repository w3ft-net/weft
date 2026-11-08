import { useState, useEffect, type FormEvent } from 'react'
import { useNavigate, useSearchParams, Link } from 'react-router-dom'
import { loadStripe, type Stripe } from '@stripe/stripe-js'
import {
  Elements,
  PaymentElement,
  useStripe,
  useElements,
} from '@stripe/react-stripe-js'
import { createSetupIntent } from '../lib/api'
import { TIER_LABELS } from '../lib/plans'
import Pane from '../components/Pane'
import './Login.css'
import './Signup.css'


// Stripe renders the Payment Element in its own iframe, so it can't see
// our CSS variables — these mirror the tokens in index.css by value.
const ELEMENTS_APPEARANCE = {
  theme: 'night' as const,
  variables: {
    colorPrimary: '#5fafd7',
    colorBackground: '#0a0a14',
    colorText: '#e0e0f0',
    colorTextSecondary: '#8a8aaa',
    colorTextPlaceholder: '#555577',
    colorDanger: '#ff5f5f',
    fontFamily: "'JetBrains Mono', ui-monospace, monospace",
    fontSizeBase: '13px',
    borderRadius: '0px',
    spacingUnit: '3px',
  },
  rules: {
    '.Input': { border: '1px solid #3a3a60', boxShadow: 'none' },
    '.Input:focus': { border: '1px solid #5fafd7', boxShadow: 'none' },
    '.Tab': { border: '1px solid #3a3a60', boxShadow: 'none' },
    '.Tab--selected': { borderColor: '#5fafd7', boxShadow: 'none' },
    '.Label': { color: '#8a8aaa' },
  },
}

function PaymentForm() {
  const stripe = useStripe()
  const elements = useElements()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!stripe || !elements) return
    setSubmitting(true)
    setError('')

    const result = await stripe.confirmSetup({
      elements,
      confirmParams: {
        return_url: `${window.location.origin}/dashboard?welcome=1`,
      },
    })

    if (result.error) {
      setError(result.error.message ?? 'Payment setup failed.')
      setSubmitting(false)
    }
  }

  return (
    <form className="login-form" onSubmit={handleSubmit} data-testid="payment-setup-form">
      <div className="signup-payment-element">
        <PaymentElement options={{ layout: 'tabs' }} />
      </div>
      {error && (
        <div className="login-error" data-testid="payment-setup-error">
          {error}
        </div>
      )}
      <div className="login-actions">
        <button
          type="submit"
          className="tui-btn tui-btn-primary login-submit"
          disabled={!stripe || submitting}
          data-testid="payment-setup-submit-button"
        >
          {submitting ? 'confirming…' : 'start free trial'}
        </button>
      </div>
      <p className="signup-disclaimer">
        no charge until the 14-day trial ends. cancel anytime.
      </p>
    </form>
  )
}

export default function PaymentSetup() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()

  const tierParam = searchParams.get('tier') || 'swatch'
  const tier = tierParam in TIER_LABELS ? tierParam : 'swatch'

  const [stripePromise, setStripePromise] = useState<Promise<Stripe | null> | null>(null)
  const [clientSecret, setClientSecret] = useState<string>()
  const [loadError, setLoadError] = useState('')

  useEffect(() => {
    createSetupIntent(tier)
      .then(({ client_secret, publishable_key }) => {
        setStripePromise(loadStripe(publishable_key))
        setClientSecret(client_secret)
      })
      .catch(err => {
        setLoadError(err instanceof Error ? err.message : 'Failed to initialize payment.')
      })
  }, [tier])

  return (
    <div className="login-page">
      <img src="/weft-logo.svg" alt="weft" className="login-logo" />
      <Pane title="payment" meta="step 2/2" className="login-card">
        <p className="login-motd">one last step</p>

        <div className="login-form">
          <div className="signup-tier" data-testid="payment-setup-tier">
            <span className="signup-tier-label">plan</span>
            <span className="signup-tier-value">{TIER_LABELS[tier]}</span>
          </div>

          {loadError && (
            <div className="login-error" data-testid="payment-setup-init-error">
              {loadError}
            </div>
          )}

          {!loadError && (!stripePromise || !clientSecret) && (
            <div className="signup-loading tui-cursor" data-testid="payment-setup-loading">loading payment form</div>
          )}

          {stripePromise && clientSecret && (
            <Elements stripe={stripePromise} options={{ clientSecret, appearance: ELEMENTS_APPEARANCE }}>
              <PaymentForm />
            </Elements>
          )}
        </div>
      </Pane>

      <div className="login-foot">
        <button
          className="login-back"
          onClick={() => navigate('/dashboards')}
          data-testid="payment-setup-skip-button"
        >
          skip, add payment later
        </button>
        <Link to="/" className="login-back" data-testid="payment-setup-home-link">
          ← back to home
        </Link>
      </div>
    </div>
  )
}
