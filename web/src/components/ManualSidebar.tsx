import { NavLink } from 'react-router-dom'
import { manualCategories, manualSections } from '../generated/docs'
import './ManualSidebar.css'

// ManualSidebar renders a grouped topic list shared by the manual
// landing page and every topic page, drawn like a TUI file tree:
// categories as directories, topics as entries, the open topic in
// inverse video. On wide viewports it sits as a persistent left rail
// inside the manual content area; on narrow viewports it collapses to a
// <details>/<summary> block at the top so the main app sidebar's
// hamburger remains the only navigation toggle.
export default function ManualSidebar() {
  return (
    <>
      <aside className="manual-sidebar manual-sidebar-wide" data-testid="manual-sidebar">
        <CategoryList />
      </aside>
      <details className="manual-sidebar manual-sidebar-narrow" data-testid="manual-sidebar-narrow">
        <summary data-testid="manual-sidebar-toggle-button">browse the manual</summary>
        <CategoryList />
      </details>
    </>
  )
}

function CategoryList() {
  return (
    <nav className="manual-sidebar-nav">
      <NavLink
        to="/manual"
        end
        className={({ isActive }) =>
          `manual-sidebar-home${isActive ? ' manual-sidebar-home-active' : ''}`
        }
        data-testid="manual-sidebar-home-link"
      >
        manual/
      </NavLink>
      {manualCategories.map(cat => {
        const topics = manualSections.filter(s => s.category === cat.id)
        if (topics.length === 0) return null
        return (
          <div key={cat.id} className="manual-sidebar-group">
            <div className="manual-sidebar-group-label">{cat.label.toLowerCase()}/</div>
            <ul className="manual-sidebar-topics">
              {topics.map((t, i) => (
                <li key={t.section}>
                  <NavLink
                    to={`/manual/${t.section}`}
                    className={({ isActive }) =>
                      `manual-sidebar-topic${isActive ? ' manual-sidebar-topic-active' : ''}`
                    }
                    data-testid={`manual-sidebar-topic-${t.section}-link`}
                  >
                    <span className="manual-sidebar-branch" aria-hidden="true">
                      {i === topics.length - 1 ? '└ ' : '├ '}
                    </span>
                    {t.title}
                  </NavLink>
                </li>
              ))}
            </ul>
          </div>
        )
      })}
    </nav>
  )
}
