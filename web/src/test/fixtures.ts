import type { Order, User } from '../types'

export const testUser: User = {
  id: '0123456789abcdef0123456789abcdef',
  email: 'user@example.test',
  created_at: '2026-11-17T09:00:00Z',
}

export function makeOrder(overrides: Partial<Order> = {}): Order {
  return {
    id: 'ad354a4459ffdff6870e77e85bd04ac0',
    status: 'created',
    clinic: { name: 'Praxis Test', phone: '+431234567' },
    patient: {
      full_name: 'Max Mustermann',
      phone: '+436641234567',
      birth_date: '1985-04-12',
      reason: 'Kontrolle',
      preferred_slots: [{ start: '2026-11-20T09:00:00Z', end: '2026-11-20T10:00:00Z' }],
    },
    outcome: null,
    recording: null,
    created_at: '2026-11-17T09:00:00Z',
    updated_at: '2026-11-17T09:00:00Z',
    attempts: 0,
    user_id: testUser.id,
    ...overrides,
  }
}
