import { useEffect, useMemo, useState } from 'react'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { getPendingSourceCount, onPendingSourcesChanged } from '../lib/api'
import './SidebarNav.css'

// Each entry gets a digit hotkey from its position (1-based), shown in
// the gutter like a tmux window index. The label doubles as the testid
// slug, so keep it a single word.
const NAV_ITEMS = [
  { to: '/dashboards', label: 'Dashboards' },
  { to: '/shell', label: 'Shell' },
  { to: '/namespaces', label: 'Namespaces' },
  { to: '/downloads', label: 'Downloads' },
  { to: '/account', label: 'Account' },
  { to: '/manual', label: 'Manual' },
]

// isTypingTarget reports whether a keydown landed somewhere the user is
// entering text — inputs, the xterm helper textarea, contenteditable —
// where digit hotkeys must not fire.
function isTypingTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false
  const tag = el.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable
}

function useClock(): string {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 30_000)
    return () => clearInterval(t)
  }, [])
  return now.toTimeString().slice(0, 5)
}

export default function SidebarNav() {
  const { user, logout, switchOrg } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const clock = useClock()
  const [pendingCount, setPendingCount] = useState(0)
  // open drives the narrow-viewport drawer behavior. The wide-
  // viewport CSS ignores this state and shows the sidebar always;
  // narrow viewport CSS slides the sidebar in when open=true.
  const [open, setOpen] = useState(false)

  useEffect(() => {
    function refresh() {
      getPendingSourceCount()
        .then(setPendingCount)
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

  const isSuperadmin = !!user?.is_superadmin
  const items = useMemo(
    () => isSuperadmin ? [...NAV_ITEMS, { to: '/admin', label: 'Admin' }] : NAV_ITEMS,
    [isSuperadmin],
  )

  // Digit hotkeys jump between windows, tmux-style. Ignored while typing
  // or with a modifier held so browser/OS shortcuts keep working.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey || isTypingTarget(e.target)) return
      const n = Number(e.key)
      if (Number.isInteger(n) && n >= 1 && n <= items.length) {
        e.preventDefault()
        navigate(items[n - 1].to)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [items, navigate])

  // Watch detail pages are reached from dashboard tiles, so they report
  // as the dashboards window.
  const path = location.pathname.startsWith('/cron/') ? '/dashboards' : location.pathname
  const current = items.find(i => path.startsWith(i.to))

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
        ≡
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

          <nav className="sidebar-links" aria-label="main">
            {items.map((item, i) => {
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
                  <span className="sidebar-link-icon">{i + 1}</span>
                  <span className="sidebar-link-label">{item.label.toLowerCase()}</span>
                  {showPendingDot && (
                    <span
                      className="sidebar-pending-dot"
                      title={`${pendingCount} source${pendingCount === 1 ? '' : 's'} awaiting configuration`}
                      data-testid="sidebar-pending-dot"
                    >
                      {pendingCount}
                    </span>
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
        <button
          className="sidebar-logout"
          onClick={handleLogout}
          data-testid="sidebar-logout-button"
        >
          sign out
        </button>
      </div>
      </aside>
      <footer className="statusbar" data-testid="statusbar">
        <span className="statusbar-seg statusbar-brand">weft</span>
        <span className="statusbar-seg statusbar-window">
          {current ? `${items.indexOf(current) + 1}:${current.label.toLowerCase()}` : '-'}
        </span>
        {user && (
          <span className="statusbar-seg statusbar-who" data-testid="sidebar-user-email">
            {user.email}<span className="statusbar-at">@</span>{user.customer}
          </span>
        )}
        <span className="statusbar-fill" />
        <span className="statusbar-seg statusbar-hints">
          <kbd className="tui-kbd">1</kbd>–<kbd className="tui-kbd">{items.length}</kbd> switch
        </span>
        <span className="statusbar-seg statusbar-clock">{clock}</span>
      </footer>
    </>
  )
}
