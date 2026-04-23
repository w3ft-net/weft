import { useEffect } from 'react'
import { useLocation } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { manualSections } from '../generated/docs'
import './Manual.css'

// Manual renders every markdown-authored section from docs/manual/
// (and any docs/shell topic that opts in via `web:` frontmatter).
// Generated HTML is injected through dangerouslySetInnerHTML — the
// docgen tool disallows raw HTML in source, so every byte rendered
// here came from parsed markdown.
export default function Manual() {
  const { hash } = useLocation()

  useEffect(() => {
    if (hash) {
      const el = document.getElementById(hash.slice(1))
      if (el) el.scrollIntoView({ behavior: 'smooth' })
    }
  }, [hash])

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="manual-content" data-testid="manual-main">
          <h1 className="manual-page-title">Manual</h1>

          <ul className="manual-toc" data-testid="manual-toc">
            {manualSections.map((s) => (
              <li key={s.section}>
                <a
                  href={`#${s.section}`}
                  data-testid={`manual-toc-${s.section}-link`}
                >
                  {s.title}
                </a>
              </li>
            ))}
          </ul>

          {manualSections.map((s) => (
            <section
              key={s.section}
              className="manual-section"
              id={s.section}
              data-testid={`manual-section-${s.section}`}
              dangerouslySetInnerHTML={{ __html: s.html }}
            />
          ))}
        </div>
      </main>
    </div>
  )
}
