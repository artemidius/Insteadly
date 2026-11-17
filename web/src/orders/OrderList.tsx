import type { Order } from '../types'

interface OrderListProps {
  orders: Order[]
  selectedId: string | null
  onSelect: (id: string) => void
}

export function OrderList({ orders, selectedId, onSelect }: OrderListProps) {
  if (orders.length === 0) {
    return <p className="muted">No orders yet. Place one on the left.</p>
  }
  return (
    <ul className="order-list" aria-label="Orders">
      {orders.map((order) => (
        <li key={order.id} className={order.id === selectedId ? 'selected' : undefined}>
          <button type="button" onClick={() => onSelect(order.id)} aria-pressed={order.id === selectedId}>
            <span className={`status status-${order.status}`}>{order.status.replace('_', ' ')}</span>
            <span className="order-clinic">{order.clinic.name}</span>
            <span className="order-patient">{order.patient.full_name}</span>
            <span className="order-date">{new Date(order.created_at).toLocaleString()}</span>
          </button>
        </li>
      ))}
    </ul>
  )
}
