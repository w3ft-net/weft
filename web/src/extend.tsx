import { createContext, useContext, type ComponentType, type ReactNode } from 'react'
import type { User } from './lib/api'

// A deployment's way into the app. The core app is the shell, dashboards,
// namespaces, the manual and panes; hosted adds its own pages and chrome by
// passing an Extension to <App>. Nothing here is hosted-specific: a build with
// no Extension is the open app.

export interface NavItem {
  to: string
  label: string // one word: it is also the testid slug
}

// What the shell page hands an extension, so it can type into the terminal.
export interface ShellHandle {
  ready: boolean
  setInput(text: string): void
  focus(): void
}

export interface Extension {
  // <Route> elements for the extension's own pages. They sit beside the core's,
  // so wrap one that needs a session in <ProtectedRoute>.
  routes?: ReactNode
  // The sidebar's entries, given the core's. Position gives an entry its digit
  // hotkey. May depend on the user (hosted shows Admin to the superadmin only).
  nav?: (user: User | null, core: NavItem[]) => NavItem[]
  // Rendered inside the signed-in app frame, above the page: banners, modals,
  // overlays.
  frame?: ComponentType
  // Rendered at the bottom of the sidebar, above sign out (an org switcher).
  sidebarFooter?: ComponentType
  // A path to send the signed-in user to instead of the page they asked for
  // (hosted: the payment step of a signup), or null to let them through.
  gate?: (user: User) => string | null
  // Called by the shell page with a handle on its terminal, as a hook: the
  // extension can type a suggested command into it.
  useShell?: (shell: ShellHandle) => void
  // How far back a cron's chart may reach for this user, in milliseconds; the
  // chart marks what lies beyond it. Absent means no limit.
  chartRetentionMs?: (user: User | null) => number | undefined
  // Where the signed-out user is sent, and where sign out lands. Default /login.
  loginPath?: string
  // Where a signed-in user lands from the root path. Default /dashboards.
  homePath?: string
}

const ExtensionContext = createContext<Extension>({})

export const ExtensionProvider = ExtensionContext.Provider
export const useExtension = (): Extension => useContext(ExtensionContext)
