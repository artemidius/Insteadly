import { useState, type FormEvent } from 'react'

import { errorMessage } from '../api'
import { useAuth } from './AuthProvider'

interface LoginProps {
  onSwitch: () => void
}

export function Login({ onSwitch }: LoginProps) {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError(null)
    if (!email.trim() || !password) {
      setError('Email and password are required')
      return
    }
    setSubmitting(true)
    try {
      await login(email.trim(), password)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form className="auth-form" onSubmit={handleSubmit} aria-label="Sign in">
      <h1>Welcome back</h1>
      <p className="muted">Sign in to place and track appointment orders.</p>
      <label htmlFor="login-email">Email</label>
      <input
        id="login-email"
        type="email"
        autoComplete="email"
        value={email}
        onChange={(event) => setEmail(event.target.value)}
      />
      <label htmlFor="login-password">Password</label>
      <input
        id="login-password"
        type="password"
        autoComplete="current-password"
        value={password}
        onChange={(event) => setPassword(event.target.value)}
      />
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <button type="submit" disabled={submitting}>
        {submitting ? 'Signing in…' : 'Sign in'}
      </button>
      <p className="switch">
        Need an account?{' '}
        <button type="button" className="link" onClick={onSwitch}>
          Create one
        </button>
      </p>
    </form>
  )
}
