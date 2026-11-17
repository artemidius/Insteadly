import { useState, type FormEvent } from 'react'

import { errorMessage } from '../api'
import { useAuth } from './AuthProvider'

interface RegisterProps {
  onSwitch: () => void
}

// Register keeps its validation local (one round trip) while the server remains the
// authority; its problem list is shown verbatim on a 400.
export function Register({ onSwitch }: RegisterProps) {
  const { register } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError(null)
    const problems: string[] = []
    if (!email.includes('@')) problems.push('Email must look like name@example.com')
    if (password.length < 8) problems.push('Password must be at least 8 characters')
    if (confirm !== password) problems.push('Passwords do not match')
    if (problems.length > 0) {
      setError(problems.join('. '))
      return
    }
    setSubmitting(true)
    try {
      await register(email.trim(), password)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form className="auth-form" onSubmit={handleSubmit} aria-label="Create account">
      <h1>Create your Insteadly account</h1>
      <p className="muted">Have an assistant book your next appointment by phone.</p>
      <label htmlFor="register-email">Email</label>
      <input
        id="register-email"
        type="email"
        autoComplete="email"
        value={email}
        onChange={(event) => setEmail(event.target.value)}
      />
      <label htmlFor="register-password">Password</label>
      <input
        id="register-password"
        type="password"
        autoComplete="new-password"
        value={password}
        onChange={(event) => setPassword(event.target.value)}
      />
      <label htmlFor="register-confirm">Confirm password</label>
      <input
        id="register-confirm"
        type="password"
        autoComplete="new-password"
        value={confirm}
        onChange={(event) => setConfirm(event.target.value)}
      />
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <button type="submit" disabled={submitting}>
        {submitting ? 'Creating account…' : 'Create account'}
      </button>
      <p className="switch">
        Already have an account?{' '}
        <button type="button" className="link" onClick={onSwitch}>
          Sign in
        </button>
      </p>
    </form>
  )
}
