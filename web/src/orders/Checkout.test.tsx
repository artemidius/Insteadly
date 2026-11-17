import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { makeOrder } from '../test/fixtures'

const apiMocks = vi.hoisted(() => ({
  checkout: vi.fn(),
}))

vi.mock('../api', () => ({
  api: { checkout: apiMocks.checkout },
  errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)),
}))

import { CheckoutButton } from './Checkout'

beforeEach(() => {
  apiMocks.checkout.mockReset()
})

describe('CheckoutButton', () => {
  it('redirects to the provider URL returned by checkout', async () => {
    apiMocks.checkout.mockResolvedValue({ redirect_url: 'https://checkout.stripe.test/pay/cs_1' })
    const onRedirect = vi.fn()
    render(<CheckoutButton order={makeOrder()} onRedirect={onRedirect} />)

    fireEvent.click(screen.getByRole('button', { name: 'Pay and schedule' }))

    await waitFor(() => {
      expect(onRedirect).toHaveBeenCalledWith('https://checkout.stripe.test/pay/cs_1')
    })
    expect(apiMocks.checkout).toHaveBeenCalledWith('ad354a4459ffdff6870e77e85bd04ac0')
  })

  it('shows an error and re-enables the button when checkout fails', async () => {
    apiMocks.checkout.mockRejectedValue(new Error('could not start checkout'))
    render(<CheckoutButton order={makeOrder()} />)

    fireEvent.click(screen.getByRole('button', { name: 'Pay and schedule' }))

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('could not start checkout'))
    expect(screen.getByRole('button', { name: 'Pay and schedule' })).toBeEnabled()
  })

  it('renders nothing once the order is paid', () => {
    const { container } = render(<CheckoutButton order={makeOrder({ status: 'paid' })} />)
    expect(container).toBeEmptyDOMElement()
  })
})
