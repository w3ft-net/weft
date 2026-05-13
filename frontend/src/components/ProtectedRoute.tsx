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

  return (
    <>
      <VerificationBanner />
      <GraduationBanner />
      <WelcomeModal />
      <TourOverlay />
      {children}
    </>
  )
}
