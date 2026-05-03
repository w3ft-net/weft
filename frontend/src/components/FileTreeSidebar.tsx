import { useEffect, useState, useCallback } from 'react'
import { listFS, type FSEntry } from '../lib/api'
import './FileTreeSidebar.css'

interface FileTreeSidebarProps {
  // root is the path the sidebar is anchored at — '/tmp' or '/etc'.
  // The tree always shows root's immediate children at the top level;
  // directory rows expand inline.
  root: string
  // open controls visibility; when false, the data fetch is skipped.
  open: boolean
  // onClose is the X button / backdrop handler.
  onClose: () => void
  // onPickFile fires when the user clicks a non-directory entry. The
  // Shell page uses this to inject `cat <path>` into the terminal
  // prompt — keeping the path-resolution logic out of this component
  // since other callers might want to open in vim instead.
  onPickFile: (absolutePath: string) => void
  // testIdPrefix scopes data-testids so the same component can serve
  // both /tmp and /etc without colliding (e.g. shell-tree-tmp-,
  // shell-tree-etc-).
  testIdPrefix: string
}

interface DirState {
  entries: FSEntry[] | null
  loading: boolean
  error: string | null
  expanded: Record<string, boolean>
}

// FileTreeSidebar renders a lazy directory tree. The root path's
// children are fetched when the sidebar opens; clicking a directory
// row toggles its expansion and fetches its children on first open.
// File rows fire onPickFile with the absolute path.
//
// The tree is shallow by design — we only support one level of
// nesting visible at a time, with the parent row collapsing as the
// user navigates. This keeps the UI usable on phone-sized screens
// without nested-scroll gymnastics. The /etc tree's only nesting
// today is /etc/dashboards, so this isn't a real limitation.
export default function FileTreeSidebar({
  root,
  open,
  onClose,
  onPickFile,
  testIdPrefix,
}: FileTreeSidebarProps) {
  const [state, setState] = useState<DirState>({
    entries: null,
    loading: false,
    error: null,
    expanded: {},
  })
  // childCache maps "<absolute path>" → entries. First click on a
  // directory fetches; subsequent clicks toggle the cached entries
  // without refetching.
  const [childCache, setChildCache] = useState<Record<string, FSEntry[]>>({})

  const refresh = useCallback(() => {
    setState(s => ({ ...s, loading: true, error: null }))
    listFS(root)
      .then(resp => setState(s => ({
        ...s,
        loading: false,
        entries: resp.entries,
      })))
      .catch(err => setState(s => ({
        ...s,
        loading: false,
        error: err.message || 'failed to list',
      })))
  }, [root])

  useEffect(() => {
    if (!open) return
    refresh()
  }, [open, refresh])

  function joinPath(base: string, name: string): string {
    return base.endsWith('/') ? base + name : base + '/' + name
  }

  function toggleDir(absPath: string) {
    setState(s => ({
      ...s,
      expanded: { ...s.expanded, [absPath]: !s.expanded[absPath] },
    }))
    // Lazy-load children on first expansion.
    if (!childCache[absPath]) {
      listFS(absPath)
        .then(resp => setChildCache(c => ({ ...c, [absPath]: resp.entries })))
        .catch(err => console.error(`list ${absPath}:`, err))
    }
  }

  return (
    <div
      className={`shell-query-sidebar${open ? ' shell-query-sidebar-open' : ''}`}
      data-testid={`${testIdPrefix}-sidebar`}
    >
      <div className="shell-query-sidebar-header">
        <span className="shell-query-sidebar-title">{root}</span>
        <button
          className="shell-query-sidebar-close"
          onClick={onClose}
          data-testid={`${testIdPrefix}-sidebar-close-button`}
        >
          ×
        </button>
      </div>
      {state.loading && state.entries === null ? (
        <div className="shell-query-empty">loading…</div>
      ) : state.error ? (
        <div className="shell-query-empty">{state.error}</div>
      ) : state.entries === null || state.entries.length === 0 ? (
        <div className="shell-query-empty">empty</div>
      ) : (
        <ul className="shell-query-list">
          {state.entries.map(entry => {
            const absPath = joinPath(root, entry.name)
            return (
              <FileTreeRow
                key={absPath}
                entry={entry}
                absPath={absPath}
                expanded={state.expanded[absPath] || false}
                children={childCache[absPath]}
                onToggleDir={toggleDir}
                onPickFile={onPickFile}
                testIdPrefix={testIdPrefix}
              />
            )
          })}
        </ul>
      )}
    </div>
  )
}

interface FileTreeRowProps {
  entry: FSEntry
  absPath: string
  expanded: boolean
  children: FSEntry[] | undefined
  onToggleDir: (absPath: string) => void
  onPickFile: (absPath: string) => void
  testIdPrefix: string
}

function FileTreeRow({
  entry,
  absPath,
  expanded,
  children,
  onToggleDir,
  onPickFile,
  testIdPrefix,
}: FileTreeRowProps) {
  const safeName = entry.name.replace(/[^a-zA-Z0-9-]/g, '_')
  return (
    <li className="file-tree-row" data-testid={`${testIdPrefix}-row-${safeName}`}>
      <button
        className={`file-tree-name${entry.is_dir ? ' file-tree-dir' : ''}`}
        onClick={() => entry.is_dir ? onToggleDir(absPath) : onPickFile(absPath)}
        data-testid={`${testIdPrefix}-${entry.is_dir ? 'dir' : 'file'}-${safeName}`}
      >
        <span className="file-tree-icon">
          {entry.is_dir ? (expanded ? '▾' : '▸') : ''}
        </span>
        {entry.name}{entry.is_dir ? '/' : ''}
      </button>
      {entry.is_dir && expanded && children && (
        <ul className="file-tree-children">
          {children.length === 0 ? (
            <li className="file-tree-empty">empty</li>
          ) : (
            children.map(child => {
              const childPath = absPath + '/' + child.name
              const childSafe = child.name.replace(/[^a-zA-Z0-9-]/g, '_')
              return (
                <li
                  key={childPath}
                  className="file-tree-row file-tree-child"
                  data-testid={`${testIdPrefix}-row-${safeName}-${childSafe}`}
                >
                  <button
                    className={`file-tree-name${child.is_dir ? ' file-tree-dir' : ''}`}
                    onClick={() => child.is_dir ? null : onPickFile(childPath)}
                    disabled={child.is_dir}
                    data-testid={`${testIdPrefix}-${child.is_dir ? 'dir' : 'file'}-${safeName}-${childSafe}`}
                  >
                    <span className="file-tree-icon">
                      {child.is_dir ? '▸' : ''}
                    </span>
                    {child.name}{child.is_dir ? '/' : ''}
                  </button>
                </li>
              )
            })
          )}
        </ul>
      )}
    </li>
  )
}
