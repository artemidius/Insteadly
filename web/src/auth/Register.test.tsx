import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { Register } from './Register'
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

function renderRegister(onSwitch = vi.fn()) {
  render(
    <AuthProvider>
      <Register onSwitch={onSwitch} />
    </AuthProvider>,
  )
  return onSwitch
}

describe('Register', () => {
  it('validates locally before calling the API', async () => {
    const fetchMock = installFetch((url) =>
      url.endsWith('/api/v1/auth/me') ? unauthenticatedMe() : json({}, 500),
    )
    renderRegister()

    fireEvent.click(screen.getByRole('button', { name: 'Create account' }))

    expect(screen.getByRole('alert')).toHaveTextContent('Email must look like name@example.com')
    expect(screen.getByRole('alert')).toHaveTextContent('Password must be at least 8 characters')
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/api/v1/auth/register'))).toBe(false)
  })

  it('reports mismatched passwords without calling the API', async () => {
    const fetchMock = installFetch((url) =>
      url.endsWith('/api/v1/auth/me') ? unauthenticatedMe() : json({}, 500),
    )
    renderRegister()

    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'new@example.test' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'password123' } })
    fireEvent.change(screen.getByLabelText('Confirm password'), { target: { value: 'different123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create account' }))

    expect(screen.getByRole('alert')).toHaveTextContent('Passwords do not match')
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/api/v1/auth/register'))).toBe(false)
  })

  it('submits the credentials and bootstraps the session', async () => {
    const fetchMock = installFetch((url) => {
      if (url.endsWith('/api/v1/auth/me')) return unauthenticatedMe()
      if (url.endsWith('/api/v1/auth/register')) return json({ user: testUser }, 201)
      return json({ error: { code: 'not_found', message: 'missing' } }, 404)
    })
    renderRegister()

    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'new@example.test' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'password123' } })
    fireEvent.change(screen.getByLabelText('Confirm password'), { target: { value: 'password123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create account' }))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/api/v1/auth/register'))).toBe(true)
    })
    const registerCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/api/v1/auth/register'))
    expect(registerCall).toBeDefined()
    const init = registerCall?.[1] as RequestInit
    expect(init.method).toBe('POST')
    expect(JSON.parse(String(init.body))).toEqual({
      email: 'new@example.test',
      password: 'password123',
    })
    expect(init.credentials).toBe('include')
  })

  it('shows a server-side conflict', async () => {
    installFetch((url) => {
      if (url.endsWith('/api/v1/auth/me')) return unauthenticatedMe()
      return json({ error: { code: 'conflict', message: 'email is already registered' } }, 409)
    })
    renderRegister()

    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'taken@example.test' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'password123' } })
    fireEvent.change(screen.getByLabelText('Confirm password'), { target: { value: 'password123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create account' }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('email is already registered')
    })
  })

  it('switches to the login view', () => {
    installFetch((url) => (url.endsWith('/api/v1/auth/me') ? unauthenticatedMe() : json({}, 500)))
    const onSwitch = renderRegister()
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(onSwitch).toHaveBeenCalledTimes(1)
  })
})
