// Mirrors the Go domain JSON (snake_case, RFC 3339 timestamps). The SPA consumes the API
// verbatim; there is deliberately no second DTO layer.

export type OrderStatus =
  | 'created'
  | 'paid'
  | 'scheduled'
  | 'dialing'
  | 'ringing'
  | 'in_call'
  | 'finished'

export type OutcomeKind = 'booked' | 'callback_needed' | 'failed' | 'aborted'

export interface TimeRange {
  start: string
  end: string
}

export interface Clinic {
  name: string
  phone: string
}

export interface Patient {
  full_name: string
  phone: string
  birth_date?: string
  insurance_number?: string
  reason: string
  preferred_slots: TimeRange[]
}

export interface RecordingFlags {
  recording: boolean | null
  transcript: boolean | null
  audio: boolean | null
}

export interface BookedSlot {
  date_time: string
  doctor: string
  practice: string
}

export interface Outcome {
  kind: OutcomeKind
  detail: string
  booked_slot?: BookedSlot
  callback_at?: string
  fail_reason?: string
}

export interface AttemptRecord {
  number: number
  outcome: Outcome
  started_at: string
  ended_at: string
}

export interface Order {
  id: string
  status: OrderStatus
  clinic: Clinic
  patient: Patient
  outcome: Outcome | null
  recording: RecordingFlags | null
  created_at: string
  updated_at: string
  attempts: number
  next_attempt_at?: string
  attempt_history?: AttemptRecord[]
  user_id?: string
  paid_at?: string
  payment_reference?: string
}

export interface OrderCreateInput {
  clinic: Clinic
  patient: Patient
}

export interface User {
  id: string
  email: string
  created_at: string
}

export interface CheckoutResponse {
  redirect_url: string
}

export interface ApiErrorBody {
  error: {
    code: string
    message: string
  }
}
