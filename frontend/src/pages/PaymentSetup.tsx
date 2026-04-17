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
import './Signup.css'

const TIER_LABELS: Record<string, string> = {
  swatch: 'Swatch — $19/mo',
  yard: 'Yard — $79/mo',
  bolt: 'Bolt — $299/mo',
}

const ELEMENTS_APPEARANCE = {
  theme: 'night' as const,
  variables: {
    colorPrimary: '#c9a96e',
    colorBackground: '#141414',
    colorText: '#e8e6e3',
    colorDanger: '#d4635a',
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    borderRadius: '4px',
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
      <PaymentElement options={{ layout: 'tabs' }} />
      {error && (
        <div className="login-error" data-testid="payment-setup-error">
          {error}
        </div>
      )}
      <button
        type="submit"
        className="login-submit"
        disabled={!stripe || submitting}
        data-testid="payment-setup-submit-button"
      >
        {submitting ? 'Confirming...' : 'Start free trial'}
      </button>
      <p className="signup-disclaimer">
        Your card won't be charged until the 14-day trial ends. Cancel anytime.
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
      <div className="login-card">
        <div className="login-logo">weft</div>
        <div className="login-tagline">one last step</div>

        <div className="signup-tier" data-testid="payment-setup-tier">
          <span className="signup-tier-label">Plan</span>
          <span className="signup-tier-value">{TIER_LABELS[tier]}</span>
        </div>

        {loadError && (
          <div className="login-error" data-testid="payment-setup-init-error">
            {loadError}
          </div>
        )}

        {!loadError && (!stripePromise || !clientSecret) && (
          <div className="dash-empty" data-testid="payment-setup-loading">Loading payment form...</div>
        )}

        {stripePromise && clientSecret && (
          <Elements stripe={stripePromise} options={{ clientSecret, appearance: ELEMENTS_APPEARANCE }}>
            <PaymentForm />
          </Elements>
        )}

        <button
          className="login-back"
          onClick={() => navigate('/dashboard')}
          data-testid="payment-setup-skip-button"
          style={{ background: 'none', border: 'none', cursor: 'pointer', marginTop: 16 }}
        >
          Skip — add payment later
        </button>
        <Link to="/" className="login-back" data-testid="payment-setup-home-link">
          ← Back to home
        </Link>
      </div>
    </div>
  )
}
