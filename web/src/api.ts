import type { ApiErrorBody, CheckoutResponse, Order, OrderCreateInput, User } from './types'

// ApiError carries the stable error-envelope code so the UI can branch on it without
// parsing messages.
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error) return error.message
  return 'Something went wrong'
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  // The session is an HttpOnly cookie; credentials:'include' also works through the
  // Vite dev proxy and a same-origin production deployment.
  const response = await fetch(path, { credentials: 'include', ...init, headers })
  if (response.status === 204) {
    return undefined as T
  }
  const text = await response.text()
  let body: unknown
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }
  if (!response.ok) {
    const envelope = body as Partial<ApiErrorBody> | undefined
    const code = envelope?.error?.code ?? 'http_error'
    const message = envelope?.error?.message ?? `request failed with status ${response.status}`
    throw new ApiError(response.status, code, message)
  }
  return body as T
}

export const api = {
  async register(email: string, password: string): Promise<User> {
    const body = await request<{ user: User }>('/api/v1/auth/register', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    })
    return body.user
  },

  async login(email: string, password: string): Promise<User> {
    const body = await request<{ user: User }>('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    })
    return body.user
  },

  logout(): Promise<void> {
    return request<void>('/api/v1/auth/logout', { method: 'POST' })
  },

  async me(): Promise<User> {
    const body = await request<{ user: User }>('/api/v1/auth/me')
    return body.user
  },

  listOrders(): Promise<Order[]> {
    return request<Order[]>('/api/v1/orders')
  },

  getOrder(id: string): Promise<Order> {
    return request<Order>(`/api/v1/orders/${encodeURIComponent(id)}`)
  },

  createOrder(input: OrderCreateInput): Promise<Order> {
    return request<Order>('/api/v1/orders', {
      method: 'POST',
      body: JSON.stringify(input),
    })
  },

  checkout(id: string): Promise<CheckoutResponse> {
    return request<CheckoutResponse>(`/api/v1/orders/${encodeURIComponent(id)}/checkout`, {
      method: 'POST',
    })
  },

  eventsURL(id: string): string {
    return `/api/v1/orders/${encodeURIComponent(id)}/events`
  },
}
