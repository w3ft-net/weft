import { type ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import VerificationBanner from './VerificationBanner'

export default function ProtectedRoute({ children }: { children: ReactNode }) {
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

  // Banner renders itself as null when email_verified !== false, so every
  // authenticated page picks up the verify-your-email prompt automatically
  // without each page having to remember to include it.
  return (
    <>
      <VerificationBanner />
      {children}
    </>
  )
}
