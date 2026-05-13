import { useEffect } from 'react'
import { Link, Navigate, useLocation, useParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import ManualSidebar from '../components/ManualSidebar'
import { manualCategories, manualSections } from '../generated/docs'
import './Manual.css'

// ManualIndex is the /manual landing page: category cards listing every
// topic. Legacy bookmarks that used the old single-page anchor format
// (`/manual#install-weftd`) are redirected to `/manual/install-weftd`
// before render so deep links keep working after the route split.
export default function ManualIndex() {
  const { hash } = useLocation()
  const legacySlug = hash.slice(1)
  if (legacySlug && manualSections.some(s => s.section === legacySlug)) {
    return <Navigate to={`/manual/${legacySlug}`} replace />
  }

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="manual-layout">
          <ManualSidebar />
          <div className="manual-body" data-testid="manual-index-main">
            <h1 className="manual-page-title">Manual</h1>
            <p className="manual-intro">
              Pick a topic. Everything here is also reachable from the
              sidebar.
            </p>
            <div className="manual-cards">
              {manualCategories.map(cat => {
                const topics = manualSections.filter(s => s.category === cat.id)
                if (topics.length === 0) return null
                const first = topics[0]
                return (
                  <Link
                    key={cat.id}
                    to={`/manual/${first.section}`}
                    className="manual-card"
                    data-testid={`manual-card-${cat.id}-link`}
                  >
                    <h2 className="manual-card-title">{cat.label}</h2>
                    <p className="manual-card-summary">{cat.summary}</p>
                    <span className="manual-card-cta">
                      Start with {first.title} →
                    </span>
                  </Link>
                )
              })}
            </div>
          </div>
        </div>
      </main>
    </div>
  )
}

// ManualTopic renders a single markdown topic. The inline TOC at the
// top is built from the heading list emitted by docgen. The hash-scroll
// useEffect resolves deep links to specific h2/h3 anchors inside the
// rendered body.
export function ManualTopic() {
  const { slug } = useParams<{ slug: string }>()
  const { hash, pathname } = useLocation()
  const topic = manualSections.find(s => s.section === slug)

  useEffect(() => {
    if (hash) {
      const el = document.getElementById(hash.slice(1))
      if (el) el.scrollIntoView({ behavior: 'smooth' })
    } else {
      window.scrollTo({ top: 0 })
    }
  }, [hash, pathname])

  if (!topic) {
    return <Navigate to="/manual" replace />
  }

  const category = manualCategories.find(c => c.id === topic.category)

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="manual-layout">
          <ManualSidebar />
          <article className="manual-body manual-topic" data-testid={`manual-topic-${topic.section}`}>
            <nav className="manual-breadcrumb" aria-label="Breadcrumb">
              <Link to="/manual" data-testid="manual-breadcrumb-home-link">Manual</Link>
              {category && <span className="manual-breadcrumb-sep"> / </span>}
              {category && <span className="manual-breadcrumb-category">{category.label}</span>}
            </nav>

            <h1 className="manual-topic-title">{topic.title}</h1>

            {topic.headings.length > 1 && (
              <nav
                className="manual-topic-toc"
                aria-label="On this page"
                data-testid="manual-topic-toc"
              >
                <div className="manual-topic-toc-label">On this page</div>
                <ul>
                  {topic.headings.map(h => (
                    <li
                      key={h.id}
                      className={`manual-topic-toc-h${h.level}`}
                    >
                      <a
                        href={`#${h.id}`}
                        data-testid={`manual-topic-toc-${h.id}-link`}
                      >
                        {h.text}
                      </a>
                    </li>
                  ))}
                </ul>
              </nav>
            )}

            <div
              className="manual-section"
              dangerouslySetInnerHTML={{ __html: topic.html }}
            />
          </article>
        </div>
      </main>
    </div>
  )
}
