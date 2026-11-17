import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { Login } from './Login'
import { AuthProvider } from './AuthProvider'
import { testUser } from '../test/fixtures'

type FetchHandler = (url: string, init?: RequestInit) => Response

function installFetch(handler: FetchHandler): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    return handler(url, init)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function json(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function unauthenticatedMe(): Response {
  return json({ error: { code: 'unauthorized', message: 'a valid session is required' } }, 401)
}

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderLogin(onSwitch = vi.fn()) {
  render(
    <AuthProvider>
      <Login onSwitch={onSwitch} />
    </AuthProvider>,
  )
  return onSwitch
}

describe('Login', () => {
  it('requires email and password before calling the API', async () => {
    const fetchMock = installFetch((url) =>
      url.endsWith('/api/v1/auth/me') ? unauthenticatedMe() : json({}, 500),
    )
    renderLogin()

    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(screen.getByRole('alert')).toHaveTextContent('Email and password are required')
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/api/v1/auth/login'))).toBe(false)
  })

  it('submits credentials to the login endpoint', async () => {
    const fetchMock = installFetch((url) => {
      if (url.endsWith('/api/v1/auth/me')) return unauthenticatedMe()
      if (url.endsWith('/api/v1/auth/login')) return json({ user: testUser }, 200)
      return json({ error: { code: 'not_found', message: 'missing' } }, 404)
    })
    renderLogin()

    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'user@example.test' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'password123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/api/v1/auth/login'))).toBe(true)
    })
    const loginCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/api/v1/auth/login'))
    expect(JSON.parse(String((loginCall?.[1] as RequestInit).body))).toEqual({
      email: 'user@example.test',
      password: 'password123',
    })
  })

  it('shows a generic bad-credentials error', async () => {
    installFetch((url) => {
      if (url.endsWith('/api/v1/auth/me')) return unauthenticatedMe()
      return json({ error: { code: 'unauthorized', message: 'invalid email or password' } }, 401)
    })
    renderLogin()

    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'user@example.test' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'wrong' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('invalid email or password')
    })
  })

  it('switches to the register view', () => {
    installFetch((url) => (url.endsWith('/api/v1/auth/me') ? unauthenticatedMe() : json({}, 500)))
    const onSwitch = renderLogin()
    fireEvent.click(screen.getByRole('button', { name: 'Create one' }))
    expect(onSwitch).toHaveBeenCalledTimes(1)
  })
})
