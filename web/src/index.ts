// What a deployment's own frontend imports from the core app (as @weft/web): the
// mount, the extension type, and the pieces its pages are built from.
export { mount } from './mount'
export { default as App } from './App'
export type { Extension, NavItem, ShellHandle } from './extend'
export { useExtension } from './extend'
export { useAuth, AuthProvider } from './lib/AuthContext'
export { default as ProtectedRoute } from './components/ProtectedRoute'
export { default as SidebarNav } from './components/SidebarNav'
export { default as Pane } from './components/Pane'
export { addRequestHeaders } from './lib/headers'
export * from './lib/api'
