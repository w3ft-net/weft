import { NavLink, useNavigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import WeftLogoSvg from './WeftLogoSvg'
import './SidebarNav.css'

const NAV_ITEMS = [
  { to: '/dashboard', label: 'Dashboard', icon: '▦' },
  { to: '/shell', label: 'Shell', icon: '>' },
  { to: '/downloads', label: 'Downloads', icon: '↓' },
  { to: '/account', label: 'Account', icon: '⚙' },
]

export default function SidebarNav() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()

  function handleLogout() {
    logout()
    navigate('/login')
  }

  return (
    <aside className="sidebar" data-testid="sidebar-nav">
      <div className="sidebar-top">
        <div className="sidebar-logo" data-testid="sidebar-logo">
          <WeftLogoSvg className="sidebar-logo-svg" />
        </div>

        <nav className="sidebar-links">
          {NAV_ITEMS.map(item => (
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
            </NavLink>
          ))}
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
