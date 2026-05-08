import { Routes, Route, Navigate } from 'react-router-dom'
import Landing from './pages/Landing'
import Login from './pages/Login'
import Signup from './pages/Signup'
import PaymentSetup from './pages/PaymentSetup'
import Verify from './pages/Verify'
import Shell from './pages/Shell'
import Downloads from './pages/Downloads'
import Account from './pages/Account'
import Device from './pages/Device'
import Namespaces from './pages/Namespaces'
import Manual from './pages/Manual'
import Watch from './pages/Watch'
import Dashboards from './pages/Dashboards'
import Admin from './pages/Admin'
import ProtectedRoute from './components/ProtectedRoute'

export default function App() {
  return (
    <Routes>
      <Route path="/" element={<Landing />} />
      <Route path="/login" element={<Login />} />
      <Route path="/signup" element={<Signup />} />
      <Route path="/signup/payment" element={
        <ProtectedRoute><PaymentSetup /></ProtectedRoute>
      } />
      <Route path="/verify" element={<Verify />} />
      <Route path="/dashboard" element={
        <ProtectedRoute><Navigate to="/dashboards" replace /></ProtectedRoute>
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
      <Route path="/manual" element={
        <ProtectedRoute><Manual /></ProtectedRoute>
      } />
      <Route path="/watch/:name" element={
        <ProtectedRoute><Watch /></ProtectedRoute>
      } />
      <Route path="/dashboards" element={
        <ProtectedRoute><Dashboards /></ProtectedRoute>
      } />
      <Route path="/dashboards/:name" element={
        <ProtectedRoute><Dashboards /></ProtectedRoute>
      } />
      <Route path="/admin" element={
        <ProtectedRoute><Admin /></ProtectedRoute>
      } />
    </Routes>
  )
}
