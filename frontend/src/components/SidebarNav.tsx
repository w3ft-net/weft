import { useEffect, useState } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { getPendingSources, onPendingSourcesChanged } from '../lib/api'
import './SidebarNav.css'

const NAV_ITEMS = [
  { to: '/dashboard', label: 'Dashboard', icon: '▦' },
  { to: '/dashboards', label: 'Dashboards', icon: '▤' },
  { to: '/shell', label: 'Shell', icon: '>' },
  { to: '/namespaces', label: 'Namespaces', icon: '◫' },
  { to: '/downloads', label: 'Downloads', icon: '↓' },
  { to: '/account', label: 'Account', icon: '⚙' },
  { to: '/manual', label: 'Manual', icon: '?' },
]

export default function SidebarNav() {
  const { user, logout, switchOrg } = useAuth()
  const navigate = useNavigate()
  const [pendingCount, setPendingCount] = useState(0)
  // open drives the narrow-viewport drawer behavior. The wide-
  // viewport CSS ignores this state and shows the sidebar always;
  // narrow viewport CSS slides the sidebar in when open=true.
  const [open, setOpen] = useState(false)

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

  // Close the drawer when the user picks a nav item — keeps mobile
  // navigation feeling like a native phone app rather than leaving a
  // 200px-wide pane covering half the screen on the destination page.
  function handleNavClick() {
    setOpen(false)
  }

  return (
    <>
      <button
        className="sidebar-hamburger"
        type="button"
        aria-label="toggle navigation"
        onClick={() => setOpen(o => !o)}
        data-testid="sidebar-hamburger-button"
      >
        ☰
      </button>
      {open && (
        <div
          className="sidebar-backdrop"
          onClick={() => setOpen(false)}
          data-testid="sidebar-backdrop"
        />
      )}
      <aside
        className={`sidebar${open ? ' sidebar-open' : ''}`}
        data-testid="sidebar-nav"
      >
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
                  onClick={handleNavClick}
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
        {user && user.orgs && user.orgs.length > 1 && (
          <label className="sidebar-org" data-testid="sidebar-org-label">
            <span className="sidebar-org-label">org</span>
            <select
              className="sidebar-org-select"
              data-testid="sidebar-org-select"
              value={user.customer}
              onChange={e => switchOrg(e.target.value)}
            >
              {user.orgs.map(o => (
                <option
                  key={o.id}
                  value={o.name}
                  data-testid={`sidebar-org-option-${o.name}`}
                >
                  {o.name}
                </option>
              ))}
            </select>
          </label>
        )}
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
    </>
  )
}
