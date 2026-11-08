import { type ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import VerificationBanner from './VerificationBanner'
import WelcomeModal from './WelcomeModal'
import TourOverlay from './TourOverlay'
import GraduationBanner from './GraduationBanner'

export default function ProtectedRoute({ children, bare }: { children: ReactNode; bare?: boolean }) {
  const { user, loading } = useAuth()

  if (loading) {
    return (
      <div style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        fontFamily: 'var(--font-mono)',
        color: 'var(--text-dim)',
        fontSize: '0.9rem',
      }}>
        loading...
      </div>
    )
  }

  if (!user) {
    return <Navigate to="/login" replace />
  }

  if (bare) return <>{children}</>

  // Signed up but no card yet: the org has no active subscription, so the only
  // thing to do is finish the payment step.
  if (user.pending_payment_tier) {
    return <Navigate to={`/signup/payment?tier=${encodeURIComponent(user.pending_payment_tier)}`} replace />
  }

  // app-frame owns the viewport height: banners take what they need at
  // the top and the page gets the rest, so pages size themselves to their
  // parent (height: 100%) rather than to 100vh — which would overflow by
  // the banners' height whenever one is showing.
  return (
    <div className="app-frame">
      <VerificationBanner />
      <GraduationBanner />
      <WelcomeModal />
      <TourOverlay />
      <div className="app-frame-body">{children}</div>
    </div>
  )
}
