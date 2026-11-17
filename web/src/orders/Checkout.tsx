import { useState } from 'react'

import { api, errorMessage } from '../api'
import type { Order } from '../types'

interface CheckoutButtonProps {
  order: Order
  // onRedirect exists so tests can assert the navigation target without touching jsdom's
  // real location. Production navigates the browser.
  onRedirect?: (url: string) => void
}

export function CheckoutButton({ order, onRedirect }: CheckoutButtonProps) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // A paid/scheduled order cannot be checked out again; the server would accept the call
  // but the UI hides it to avoid a confusing second payment attempt.
  if (order.status !== 'created') {
    return null
  }

  async function startCheckout() {
    setBusy(true)
    setError(null)
    try {
      const { redirect_url: redirectURL } = await api.checkout(order.id)
      if (onRedirect) {
        onRedirect(redirectURL)
      } else {
        window.location.assign(redirectURL)
      }
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  return (
    <div className="checkout">
      <button type="button" onClick={startCheckout} disabled={busy}>
        {busy ? 'Starting checkout…' : 'Pay and schedule'}
      </button>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
    </div>
  )
}
