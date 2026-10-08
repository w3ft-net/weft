import { useEffect, useRef } from 'react'
import { Link, Navigate, useLocation, useParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import ManualSidebar from '../components/ManualSidebar'
import { manualCategories, manualSections } from '../generated/docs'
import './Manual.css'

// ManualIndex is the /manual landing page, laid out like `whatis`
// output: each category with its summary, then its topics one per line.
// Legacy bookmarks that used the old single-page anchor format
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
            <ManHeader center="weft manual" />
            <h1 className="manual-page-title">manual</h1>
            <p className="manual-intro">
              Pick a topic. Everything here is also in the list on the left.
            </p>
            <div className="manual-cards">
              {manualCategories.map(cat => {
                const topics = manualSections.filter(s => s.category === cat.id)
                if (topics.length === 0) return null
                const first = topics[0]
                return (
                  <section key={cat.id} className="manual-card">
                    <h2 className="manual-card-title">
                      <Link
                        to={`/manual/${first.section}`}
                        data-testid={`manual-card-${cat.id}-link`}
                      >
                        {cat.label.toLowerCase()}
                      </Link>
                    </h2>
                    <p className="manual-card-summary">{cat.summary}</p>
                    <ul className="manual-card-topics">
                      {topics.map(t => (
                        <li key={t.section}>
                          <Link
                            to={`/manual/${t.section}`}
                            className="manual-card-topic"
                            data-testid={`manual-card-topic-${t.section}-link`}
                          >
                            <span className="manual-card-topic-slug">{t.section}</span>
                            <span className="manual-card-topic-title">{t.title}</span>
                          </Link>
                        </li>
                      ))}
                    </ul>
                  </section>
                )
              })}
            </div>
          </div>
        </div>
      </main>
    </div>
  )
}

// ManHeader is the man(1) running header: page name on both ends, the
// manual's name in the middle.
function ManHeader({ left = 'weft(1)', center }: { left?: string; center: string }) {
  return (
    <div className="manual-manhead" aria-hidden="true">
      <span>{left}</span>
      <span>{center}</span>
      <span>{left}</span>
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
  const mainRef = useRef<HTMLElement>(null)

  useEffect(() => {
    if (hash) {
      const el = document.getElementById(hash.slice(1))
      if (el) el.scrollIntoView({ behavior: 'smooth' })
    } else {
      // The page scrolls inside .app-main, not the window.
      mainRef.current?.scrollTo({ top: 0 })
    }
  }, [hash, pathname])

  if (!topic) {
    return <Navigate to="/manual" replace />
  }

  const category = manualCategories.find(c => c.id === topic.category)

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" ref={mainRef}>
        <div className="manual-layout">
          <ManualSidebar />
          <article className="manual-body manual-topic" data-testid={`manual-topic-${topic.section}`}>
            <ManHeader left={`${topic.section}(1)`} center={category ? category.label.toLowerCase() : 'weft manual'} />

            <nav className="manual-breadcrumb" aria-label="Breadcrumb">
              <span className="manual-breadcrumb-sep">~/</span>
              <Link to="/manual" data-testid="manual-breadcrumb-home-link">manual</Link>
              {category && <span className="manual-breadcrumb-sep">/</span>}
              {category && <span className="manual-breadcrumb-category">{category.label.toLowerCase()}</span>}
            </nav>

            <h1 className="manual-topic-title">{topic.title}</h1>

            {topic.headings.length > 1 && (
              <nav
                className="manual-topic-toc"
                aria-label="On this page"
                data-testid="manual-topic-toc"
              >
                <div className="manual-topic-toc-label">sections</div>
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

            <div className="manual-end" aria-hidden="true">
              <span className="manual-end-mark">(END)</span>
              <span className="manual-end-hint">manual page {topic.section}</span>
            </div>
          </article>
        </div>
      </main>
    </div>
  )
}
