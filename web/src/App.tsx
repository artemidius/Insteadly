import { useState } from 'react'

import { Login } from './auth/Login'
import { Register } from './auth/Register'
import { useAuth } from './auth/AuthProvider'
import { Dashboard } from './dashboard/Dashboard'

// Three views, no client router: auth (login/register) -> dashboard (order form, list,
// detail with live SSE). The URL query carries the selected order after a checkout
// redirect; everything else is component state.
export default function App() {
  const { user, loading } = useAuth()
  const [mode, setMode] = useState<'login' | 'register'>('login')

  if (loading) {
    return (
      <main className="centered">
        <p className="muted">Loading Insteadly…</p>
      </main>
    )
  }
  if (!user) {
    return (
      <main className="centered">
        {mode === 'login' ? (
          <Login onSwitch={() => setMode('register')} />
        ) : (
          <Register onSwitch={() => setMode('login')} />
        )}
      </main>
    )
  }
  return <Dashboard />
}
