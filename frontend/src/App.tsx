import { Routes, Route } from 'react-router-dom'
import Landing from './pages/Landing'
import Login from './pages/Login'
import Verify from './pages/Verify'
import Dashboard from './pages/Dashboard'
import Shell from './pages/Shell'
import Downloads from './pages/Downloads'
import Account from './pages/Account'
import Device from './pages/Device'
import Namespaces from './pages/Namespaces'
import ProtectedRoute from './components/ProtectedRoute'

export default function App() {
  return (
    <Routes>
      <Route path="/" element={<Landing />} />
      <Route path="/login" element={<Login />} />
      <Route path="/verify" element={<Verify />} />
      <Route path="/dashboard" element={
        <ProtectedRoute><Dashboard /></ProtectedRoute>
      } />
      <Route path="/shell" element={
        <ProtectedRoute><Shell /></ProtectedRoute>
      } />
      <Route path="/downloads" element={
        <ProtectedRoute><Downloads /></ProtectedRoute>
      } />
      <Route path="/namespaces" element={
        <ProtectedRoute><Namespaces /></ProtectedRoute>
      } />
      <Route path="/account" element={
        <ProtectedRoute><Account /></ProtectedRoute>
      } />
      <Route path="/device" element={
        <ProtectedRoute><Device /></ProtectedRoute>
      } />
    </Routes>
  )
}
