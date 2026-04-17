import { Routes, Route } from 'react-router-dom'
import Landing from './pages/Landing'
import LandingDraftA from './pages/LandingDraftA'
import LandingDraftB from './pages/LandingDraftB'
import LandingDraftC from './pages/LandingDraftC'
import Login from './pages/Login'
import Signup from './pages/Signup'
import PaymentSetup from './pages/PaymentSetup'
import Verify from './pages/Verify'
import Dashboard from './pages/Dashboard'
import Shell from './pages/Shell'
import Downloads from './pages/Downloads'
import Account from './pages/Account'
import Device from './pages/Device'
import Namespaces from './pages/Namespaces'
import Docs from './pages/Docs'
import ProtectedRoute from './components/ProtectedRoute'

export default function App() {
  return (
    <Routes>
      <Route path="/" element={<Landing />} />
      <Route path="/draft-a" element={<LandingDraftA />} />
      <Route path="/draft-b" element={<LandingDraftB />} />
      <Route path="/draft-c" element={<LandingDraftC />} />
      <Route path="/login" element={<Login />} />
      <Route path="/signup" element={<Signup />} />
      <Route path="/signup/payment" element={
        <ProtectedRoute><PaymentSetup /></ProtectedRoute>
      } />
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
      <Route path="/docs" element={
        <ProtectedRoute><Docs /></ProtectedRoute>
      } />
    </Routes>
  )
}
