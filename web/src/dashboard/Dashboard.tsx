import { useCallback, useEffect, useState } from 'react'

import { api, errorMessage } from '../api'
import { useAuth } from '../auth/AuthProvider'
import { OrderDetail } from '../orders/OrderDetail'
import { OrderForm } from '../orders/OrderForm'
import { OrderList } from '../orders/OrderList'
import type { Order } from '../types'

function initialSelectedOrder(): string | null {
  return new URLSearchParams(window.location.search).get('order')
}

export function Dashboard() {
  const { user, logout } = useAuth()
  const [orders, setOrders] = useState<Order[]>([])
  const [selectedId, setSelectedId] = useState<string | null>(initialSelectedOrder)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)

  const loadOrders = useCallback(async () => {
    try {
      setOrders(await api.listOrders())
      setLoadError(null)
    } catch (err) {
      setLoadError(errorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadOrders()
  }, [loadOrders])

  function selectOrder(id: string) {
    setSelectedId(id)
    window.history.replaceState(null, '', `/dashboard?order=${encodeURIComponent(id)}`)
  }

  function handleCreated(order: Order) {
    setOrders((previous) => [order, ...previous])
    selectOrder(order.id)
  }

  function handleOrderUpdated(updated: Order) {
    setOrders((previous) => previous.map((order) => (order.id === updated.id ? updated : order)))
  }

  async function handleLogout() {
    await logout()
  }

  const selected = orders.find((order) => order.id === selectedId) ?? null

  return (
    <div className="dashboard">
      <header className="topbar">
        <div>
          <h1>Insteadly</h1>
          <p className="muted">Your phone assistant for medical appointments</p>
        </div>
        <div className="account">
          <span>{user?.email}</span>
          <button type="button" onClick={handleLogout}>
            Sign out
          </button>
        </div>
      </header>

      <main className="layout">
        <section aria-label="New order">
          <h2>New order</h2>
          <OrderForm onCreated={handleCreated} />
        </section>

        <section aria-label="Orders">
          <h2>Your orders</h2>
          {loading ? (
            <p className="muted">Loading…</p>
          ) : loadError ? (
            <p className="error" role="alert">
              {loadError}
            </p>
          ) : (
            <OrderList orders={orders} selectedId={selectedId} onSelect={selectOrder} />
          )}
        </section>

        <section aria-label="Order detail">
          <h2>Order detail</h2>
          {selected ? (
            <OrderDetail order={selected} onOrder={handleOrderUpdated} />
          ) : (
            <p className="muted">Select an order to see its live status.</p>
          )}
        </section>
      </main>
    </div>
  )
}
