import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import './index.css'
import App from './App'
import { AuthProvider } from './lib/AuthContext'
import type { Extension } from './extend'

// mount renders the app into #root. The open build calls it bare; a deployment
// calls it with its Extension.
export function mount(extension?: Extension): void {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <BrowserRouter>
        <AuthProvider>
          <App extension={extension} />
        </AuthProvider>
      </BrowserRouter>
    </StrictMode>,
  )
}
