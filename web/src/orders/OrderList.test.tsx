import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { makeOrder } from '../test/fixtures'
import { OrderList } from './OrderList'

describe('OrderList', () => {
  it('shows an empty state', () => {
    render(<OrderList orders={[]} selectedId={null} onSelect={vi.fn()} />)
    expect(screen.getByText('No orders yet. Place one on the left.')).toBeInTheDocument()
  })

  it('renders status and selects an order', () => {
    const first = makeOrder({ id: 'first', status: 'created' })
    const second = makeOrder({ id: 'second', status: 'scheduled', clinic: { name: 'Praxis Two', phone: '+432' } })
    const onSelect = vi.fn()
    render(<OrderList orders={[first, second]} selectedId="first" onSelect={onSelect} />)

    expect(screen.getByText('created')).toBeInTheDocument()
    expect(screen.getByText('scheduled')).toBeInTheDocument()
    expect(screen.getByText('Praxis Two')).toBeInTheDocument()

    const secondButton = screen.getByRole('button', { name: /scheduled/i })
    expect(secondButton).toHaveAttribute('aria-pressed', 'false')
    fireEvent.click(secondButton)
    expect(onSelect).toHaveBeenCalledWith('second')
  })
})
