import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { makeOrder } from '../test/fixtures'
import { OrderDetail } from './OrderDetail'

class FakeEventSource {
  static instances: FakeEventSource[] = []

  readonly url: string
  closed = false
  private listeners = new Map<string, ((event: Event) => void)[]>()

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    const callback =
      typeof listener === 'function' ? listener : (event: Event) => listener.handleEvent(event)
    const existing = this.listeners.get(type) ?? []
    existing.push(callback)
    this.listeners.set(type, existing)
  }

  removeEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    const existing = this.listeners.get(type) ?? []
    this.listeners.set(
      type,
      existing.filter((callback) => callback !== listener),
    )
  }

  close() {
    this.closed = true
  }

  emit(type: string, data?: unknown) {
    const event = { data: data === undefined ? undefined : JSON.stringify(data) } as MessageEvent
    for (const callback of this.listeners.get(type) ?? []) {
      callback(event as unknown as Event)
    }
  }
}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('OrderDetail', () => {
  it('subscribes to the order SSE stream and applies live events', async () => {
    const order = makeOrder()
    const onOrder = vi.fn()
    render(<OrderDetail order={order} onOrder={onOrder} />)

    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    const source = FakeEventSource.instances[0]
    expect(source.url).toBe(`/api/v1/orders/${order.id}/events`)

    act(() => source.emit('open'))
    expect(screen.getByRole('status')).toHaveTextContent('Live updates connected')

    const paid = makeOrder({ status: 'paid', paid_at: '2026-11-17T09:05:00Z' })
    act(() => source.emit('order', paid))
    expect(screen.getByText('paid')).toBeInTheDocument()
    expect(onOrder).toHaveBeenCalledWith(paid)

    const scheduled = makeOrder({
      status: 'scheduled',
      paid_at: '2026-11-17T09:05:00Z',
      next_attempt_at: '2026-11-17T09:10:00Z',
      attempts: 0,
    })
    act(() => source.emit('order', scheduled))
    expect(screen.getByText('scheduled')).toBeInTheDocument()
    expect(screen.getByText('Next attempt')).toBeInTheDocument()
  })

  it('reports a disconnected stream and reconnects via the browser', async () => {
    render(<OrderDetail order={makeOrder()} />)
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    act(() => FakeEventSource.instances[0].emit('error'))
    expect(screen.getByRole('status')).toHaveTextContent('Live updates disconnected')
  })

  it('closes the stream on unmount', async () => {
    const { unmount } = render(<OrderDetail order={makeOrder()} />)
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    const source = FakeEventSource.instances[0]
    unmount()
    expect(source.closed).toBe(true)
  })

  it('renders a booked outcome', () => {
    render(
      <OrderDetail
        order={makeOrder({
          status: 'finished',
          outcome: {
            kind: 'booked',
            detail: 'booked',
            booked_slot: {
              date_time: '2026-11-20T09:15:00Z',
              doctor: 'Dr. Huber',
              practice: 'Praxis Test',
            },
          },
          attempts: 1,
        })}
      />,
    )
    expect(screen.getByText(/Booked .*Dr\. Huber/)).toBeInTheDocument()
  })
})
