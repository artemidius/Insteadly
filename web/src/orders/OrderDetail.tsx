import { useEffect, useRef, useState } from 'react'

import { api } from '../api'
import type { Order } from '../types'
import { CheckoutButton } from './Checkout'

interface OrderDetailProps {
  order: Order
  onOrder?: (order: Order) => void
}

type StreamState = 'connecting' | 'live' | 'error'

function formatTime(value?: string): string {
  if (!value) return '—'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString()
}

function outcomeText(order: Order): string {
  if (!order.outcome) return 'No outcome yet'
  const outcome = order.outcome
  switch (outcome.kind) {
    case 'booked':
      return `Booked ${formatTime(outcome.booked_slot?.date_time)}${
        outcome.booked_slot?.doctor ? ` with ${outcome.booked_slot.doctor}` : ''
      }`
    case 'callback_needed':
      return `Callback needed at ${formatTime(outcome.callback_at)}`
    case 'failed':
      return `Failed (${outcome.fail_reason ?? 'unknown'}): ${outcome.detail}`
    case 'aborted':
      return `Aborted: ${outcome.detail}`
    default:
      return outcome.detail
  }
}

export function OrderDetail({ order: initialOrder, onOrder }: OrderDetailProps) {
  const [order, setOrder] = useState<Order>(initialOrder)
  const [streamState, setStreamState] = useState<StreamState>('connecting')
  const onOrderRef = useRef(onOrder)

  useEffect(() => {
    onOrderRef.current = onOrder
  }, [onOrder])

  useEffect(() => {
    setOrder(initialOrder)
  }, [initialOrder])

  useEffect(() => {
    const source = new EventSource(api.eventsURL(initialOrder.id))
    const handleOrder = (event: Event) => {
      try {
        const next = JSON.parse((event as MessageEvent<string>).data) as Order
        if (!next || !next.id) return
        setOrder(next)
        setStreamState('live')
        onOrderRef.current?.(next)
      } catch {
        // A malformed frame is ignored; the stream reconnects and re-snapshots.
      }
    }
    const handleOpen = () => setStreamState('live')
    const handleError = () => setStreamState('error')
    source.addEventListener('order', handleOrder)
    source.addEventListener('open', handleOpen)
    source.addEventListener('error', handleError)
    return () => source.close()
  }, [initialOrder.id])

  return (
    <article className="order-detail" aria-label="Order detail">
      <header>
        <h3>{order.clinic.name}</h3>
        <span className={`status status-${order.status}`}>{order.status.replace('_', ' ')}</span>
      </header>
      <p className="stream" role="status">
        {streamState === 'error'
          ? 'Live updates disconnected; reconnecting…'
          : streamState === 'live'
            ? 'Live updates connected'
            : 'Connecting to live updates…'}
      </p>

      <dl>
        <dt>Patient</dt>
        <dd>
          {order.patient.full_name}
          {order.patient.phone ? ` · ${order.patient.phone}` : ''}
        </dd>
        <dt>Clinic phone</dt>
        <dd>{order.clinic.phone}</dd>
        <dt>Reason</dt>
        <dd>{order.patient.reason || '—'}</dd>
        <dt>Preferred slots</dt>
        <dd>
          {order.patient.preferred_slots.length === 0
            ? 'Flexible'
            : order.patient.preferred_slots.map((slot, index) => (
                <span key={`${slot.start}-${index}`} className="slot">
                  {formatTime(slot.start)} – {formatTime(slot.end)}
                </span>
              ))}
        </dd>
        <dt>Created</dt>
        <dd>{formatTime(order.created_at)}</dd>
        <dt>Paid</dt>
        <dd>{formatTime(order.paid_at)}</dd>
        <dt>Attempts</dt>
        <dd>{order.attempts}</dd>
        <dt>Outcome</dt>
        <dd>{outcomeText(order)}</dd>
        {order.next_attempt_at && (
          <>
            <dt>Next attempt</dt>
            <dd>{formatTime(order.next_attempt_at)}</dd>
          </>
        )}
        <dt>Order ID</dt>
        <dd>
          <code>{order.id}</code>
        </dd>
      </dl>

      {order.status === 'created' && <CheckoutButton order={order} />}
    </article>
  )
}
