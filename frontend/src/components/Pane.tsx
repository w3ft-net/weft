import type { ReactNode, HTMLAttributes } from 'react'

// Pane is the box-drawn container every page builds from. The title (and
// optional actions) sit in the top border, the way TUI frameworks draw a
// titled frame:
//
//   ┌─ team  2/5 seats ───────────── [ invite ] ┐
//   │ ...                                        │
//   └────────────────────────────────────────────┘
//
// Styling lives in index.css (.tui-pane*) so pages that can't use the
// component can still opt into the look with class names.
interface PaneProps extends Omit<HTMLAttributes<HTMLElement>, 'title'> {
  title?: ReactNode
  meta?: ReactNode
  actions?: ReactNode
  children?: ReactNode
}

export default function Pane({ title, meta, actions, children, className, ...rest }: PaneProps) {
  return (
    <section className={`tui-pane${className ? ` ${className}` : ''}`} {...rest}>
      {(title || actions) && (
        <div className="tui-pane-head">
          {title ? (
            <span className="tui-pane-title">
              {title}
              {meta && <span className="tui-pane-meta">{meta}</span>}
            </span>
          ) : <span />}
          {actions && <span className="tui-pane-actions">{actions}</span>}
        </div>
      )}
      {children}
    </section>
  )
}
