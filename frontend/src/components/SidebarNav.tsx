import { useEffect, useState } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { getPendingSources, onPendingSourcesChanged } from '../lib/api'
import './SidebarNav.css'

const NAV_ITEMS = [
  { to: '/dashboard', label: 'Dashboard', icon: '▦' },
  { to: '/shell', label: 'Shell', icon: '>' },
  { to: '/namespaces', label: 'Namespaces', icon: '◫' },
  { to: '/downloads', label: 'Downloads', icon: '↓' },
  { to: '/account', label: 'Account', icon: '⚙' },
  { to: '/docs', label: 'Docs', icon: '?' },
]

export default function SidebarNav() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()
  const [pendingCount, setPendingCount] = useState(0)

  useEffect(() => {
    function refresh() {
      getPendingSources()
        .then(s => setPendingCount(s.length))
        .catch(() => {})
    }
    refresh()
    const unsubscribe = onPendingSourcesChanged(refresh)
    // Poll occasionally to pick up sources arriving from weftd while the
    // user is sitting on another page.
    const interval = setInterval(refresh, 30_000)
    return () => {
      unsubscribe()
      clearInterval(interval)
    }
  }, [])

  function handleLogout() {
    logout()
    navigate('/login')
  }

  return (
    <aside className="sidebar" data-testid="sidebar-nav">
      <div className="sidebar-top">
        <div className="sidebar-logo" data-testid="sidebar-logo">
          <img src="/weft-logo.svg" alt="weft" className="sidebar-logo-svg" data-testid="weft-logo-svg" />
        </div>

        <nav className="sidebar-links">
          {NAV_ITEMS.map(item => {
            const showPendingDot = item.to === '/namespaces' && pendingCount > 0
            return (
              <NavLink
                key={item.to}
                to={item.to}
                className={({ isActive }) =>
                  `sidebar-link${isActive ? ' sidebar-link-active' : ''}`
                }
                data-testid={`sidebar-${item.label.toLowerCase()}-link`}
              >
                <span className="sidebar-link-icon">{item.icon}</span>
                <span className="sidebar-link-label">{item.label}</span>
                {showPendingDot && (
                  <span
                    className="sidebar-pending-dot"
                    title={`${pendingCount} source${pendingCount === 1 ? '' : 's'} awaiting configuration`}
                    data-testid="sidebar-pending-dot"
                  />
                )}
              </NavLink>
            )
          })}
        </nav>
      </div>

      <div className="sidebar-bottom">
        {user && (
          <div className="sidebar-email" data-testid="sidebar-user-email">
            {user.email}
          </div>
        )}
        <button
          className="sidebar-logout"
          onClick={handleLogout}
          data-testid="sidebar-logout-button"
        >
          sign out
        </button>
      </div>
    </aside>
  )
}
