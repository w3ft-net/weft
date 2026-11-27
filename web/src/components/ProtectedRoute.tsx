import { type ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { useExtension } from '../extend'

export default function ProtectedRoute({ children, bare }: { children: ReactNode; bare?: boolean }) {
  const { user, loading } = useAuth()
  const ext = useExtension()
  const Frame = ext.frame

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
    return <Navigate to={ext.loginPath ?? '/login'} replace />
  }

  if (bare) return <>{children}</>

  // An extension may have somewhere the user must go first (hosted: a signup
  // that has no card behind it yet goes to the payment step).
  const redirect = ext.gate?.(user)
  if (redirect) return <Navigate to={redirect} replace />

  // app-frame owns the viewport height: whatever the extension puts above the
  // page (banners) takes what it needs and the page gets the rest, so pages size
  // themselves to their parent (height: 100%) rather than to 100vh, which would
  // overflow by the banners' height whenever one is showing.
  return (
    <div className="app-frame">
      {Frame && <Frame />}
      <div className="app-frame-body">{children}</div>
    </div>
  )
}
