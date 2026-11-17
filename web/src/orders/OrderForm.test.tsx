import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { makeOrder } from '../test/fixtures'

const apiMocks = vi.hoisted(() => ({
  createOrder: vi.fn(),
  checkout: vi.fn(),
}))

vi.mock('../api', () => ({
  api: {
    createOrder: apiMocks.createOrder,
    checkout: apiMocks.checkout,
  },
  errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)),
}))

import { OrderForm } from './OrderForm'

beforeEach(() => {
  apiMocks.createOrder.mockReset()
  apiMocks.checkout.mockReset()
})

describe('OrderForm', () => {
  it('requires the clinic and patient fields', () => {
    render(<OrderForm />)
    fireEvent.click(screen.getByRole('button', { name: 'Place order' }))

    expect(screen.getAllByText('Clinic name is required')).toHaveLength(1)
    expect(screen.getAllByText('Clinic phone is required')).toHaveLength(1)
    expect(screen.getAllByText('Patient full name is required')).toHaveLength(1)
    expect(apiMocks.createOrder).not.toHaveBeenCalled()
  })

  it('rejects an inverted preferred slot', () => {
    render(<OrderForm />)
    fireEvent.change(screen.getByLabelText('Clinic name'), { target: { value: 'Praxis' } })
    fireEvent.change(screen.getByLabelText('Clinic phone'), { target: { value: '+431' } })
    fireEvent.change(screen.getByLabelText('Patient full name'), { target: { value: 'Max' } })
    fireEvent.change(screen.getByLabelText('Preferred slot start (optional)'), {
      target: { value: '2026-11-20T10:00' },
    })
    fireEvent.change(screen.getByLabelText('Preferred slot end (optional)'), {
      target: { value: '2026-11-20T09:00' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Place order' }))

    expect(screen.getByText('Preferred slot start must be before end')).toBeInTheDocument()
    expect(apiMocks.createOrder).not.toHaveBeenCalled()
  })

  it('maps the form onto the API order body and reports the created order', async () => {
    const created = makeOrder()
    apiMocks.createOrder.mockResolvedValue(created)
    const onCreated = vi.fn()
    render(<OrderForm onCreated={onCreated} />)

    fireEvent.change(screen.getByLabelText('Clinic name'), { target: { value: ' Praxis Test ' } })
    fireEvent.change(screen.getByLabelText('Clinic phone'), { target: { value: '+431234567' } })
    fireEvent.change(screen.getByLabelText('Patient full name'), { target: { value: 'Max Mustermann' } })
    fireEvent.change(screen.getByLabelText('Patient phone (optional)'), {
      target: { value: '+436641234567' },
    })
    fireEvent.change(screen.getByLabelText('Birth date (optional)'), { target: { value: '1985-04-12' } })
    fireEvent.change(screen.getByLabelText('Reason (optional)'), { target: { value: 'Kontrolle' } })
    fireEvent.change(screen.getByLabelText('Preferred slot start (optional)'), {
      target: { value: '2026-11-20T09:00' },
    })
    fireEvent.change(screen.getByLabelText('Preferred slot end (optional)'), {
      target: { value: '2026-11-20T10:00' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Place order' }))

    await waitFor(() => expect(apiMocks.createOrder).toHaveBeenCalledTimes(1))
    expect(apiMocks.createOrder).toHaveBeenCalledWith({
      clinic: { name: 'Praxis Test', phone: '+431234567' },
      patient: {
        full_name: 'Max Mustermann',
        phone: '+436641234567',
        birth_date: '1985-04-12',
        reason: 'Kontrolle',
        preferred_slots: [
          {
            start: new Date('2026-11-20T09:00').toISOString(),
            end: new Date('2026-11-20T10:00').toISOString(),
          },
        ],
      },
    })
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created))
  })

  it('shows an API failure', async () => {
    apiMocks.createOrder.mockRejectedValue(new Error('clinic.phone is invalid'))
    render(<OrderForm />)
    fireEvent.change(screen.getByLabelText('Clinic name'), { target: { value: 'Praxis' } })
    fireEvent.change(screen.getByLabelText('Clinic phone'), { target: { value: '+431' } })
    fireEvent.change(screen.getByLabelText('Patient full name'), { target: { value: 'Max' } })
    fireEvent.click(screen.getByRole('button', { name: 'Place order' }))

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('clinic.phone is invalid'))
  })
})
