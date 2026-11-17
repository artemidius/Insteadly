import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, api, errorMessage } from './api'
import { makeOrder, testUser } from './test/fixtures'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function errorResponse(status: number, code: string, message: string): Response {
  return jsonResponse({ error: { code, message } }, status)
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('api fetch wrapper', () => {
  it('decodes JSON and sends the session cookie', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse([makeOrder()]))
    vi.stubGlobal('fetch', fetchMock)

    const orders = await api.listOrders()

    expect(orders).toHaveLength(1)
    expect(orders[0].clinic.name).toBe('Praxis Test')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/v1/orders')
    expect(init.credentials).toBe('include')
  })

  it('maps the error envelope onto ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(errorResponse(401, 'unauthorized', 'a valid session is required')))

    await expect(api.me()).rejects.toMatchObject({
      name: 'ApiError',
      status: 401,
      code: 'unauthorized',
      message: 'a valid session is required',
    })
  })

  it('falls back when the error body is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('boom', { status: 500 })))

    const failure = await api.listOrders().catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(ApiError)
    expect((failure as ApiError).status).toBe(500)
    expect((failure as ApiError).code).toBe('http_error')
  })

  it('handles a 204 without a body', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })))
    await expect(api.logout()).resolves.toBeUndefined()
  })

  it('posts the order body as JSON', async () => {
    const order = makeOrder()
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(order, 201))
    vi.stubGlobal('fetch', fetchMock)

    const created = await api.createOrder({
      clinic: order.clinic,
      patient: order.patient,
    })

    expect(created.id).toBe(order.id)
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/v1/orders')
    expect(init.method).toBe('POST')
    expect(new Headers(init.headers).get('Content-Type')).toBe('application/json')
    expect(JSON.parse(String(init.body))).toMatchObject({
      clinic: order.clinic,
      patient: { full_name: order.patient.full_name },
    })
  })

  it('requests checkout with POST and returns the redirect URL', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ redirect_url: '/pay' }))
    vi.stubGlobal('fetch', fetchMock)
    const result = await api.checkout('abc')
    expect(result.redirect_url).toBe('/pay')
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/v1/orders/abc/checkout')
    expect(init.method).toBe('POST')
  })

  it('builds the SSE URL', () => {
    expect(api.eventsURL('abc')).toBe('/api/v1/orders/abc/events')
  })

  it('errorMessage extracts a useful message', () => {
    expect(errorMessage(new ApiError(400, 'bad_request', 'nope'))).toBe('nope')
    expect(errorMessage(new Error('boom'))).toBe('boom')
    expect(errorMessage('weird')).toBe('Something went wrong')
  })
})

describe('user fixture', () => {
  it('is a registered account shape', () => {
    expect(testUser.email).toContain('@')
  })
})
