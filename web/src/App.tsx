import { Routes, Route, Navigate } from 'react-router-dom'
import Login from './pages/Login'
import Verify from './pages/Verify'
import Shell from './pages/Shell'
import Panes from './pages/Panes'
import Namespaces from './pages/Namespaces'
import Manual, { ManualTopic } from './pages/Manual'
import Watch from './pages/Watch'
import Dashboards from './pages/Dashboards'
import ProtectedRoute from './components/ProtectedRoute'
import { ExtensionProvider, useExtension, type Extension } from './extend'

// Core's pages, with the extension's alongside. The extension's routes come
// first, so one that claims a path the core also answers (hosted's landing page
// at "/") wins.
export default function App({ extension = {} }: { extension?: Extension }) {
  return (
    <ExtensionProvider value={extension}>
      <AppRoutes />
    </ExtensionProvider>
  )
}

function AppRoutes() {
  const ext = useExtension()
  return (
    <Routes>
      {ext.routes}
      <Route path="/" element={<Navigate to={ext.homePath ?? '/dashboards'} replace />} />
      <Route path="/login" element={<Login />} />
      <Route path="/verify" element={<Verify />} />
      <Route path="/dashboard" element={<Navigate to="/dashboards" replace />} />
      <Route path="/shell" element={
        <ProtectedRoute><Shell /></ProtectedRoute>
      } />
      <Route path="/s/:slug" element={
        <ProtectedRoute><Shell /></ProtectedRoute>
      } />
      <Route path="/panes" element={
        <ProtectedRoute><Panes /></ProtectedRoute>
      } />
      <Route path="/namespaces" element={
        <ProtectedRoute><Namespaces /></ProtectedRoute>
      } />
      <Route path="/manual" element={
        <ProtectedRoute><Manual /></ProtectedRoute>
      } />
      <Route path="/manual/:slug" element={
        <ProtectedRoute><ManualTopic /></ProtectedRoute>
      } />
      <Route path="/cron/:name" element={
        <ProtectedRoute><Watch /></ProtectedRoute>
      } />
      <Route path="/cron/:ns/:name" element={
        <ProtectedRoute><Watch /></ProtectedRoute>
      } />
      <Route path="/dashboards" element={
        <ProtectedRoute><Dashboards /></ProtectedRoute>
      } />
      <Route path="/dashboards/:name" element={
        <ProtectedRoute><Dashboards /></ProtectedRoute>
      } />
    </Routes>
  )
}
