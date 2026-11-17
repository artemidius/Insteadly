import { useState, type FormEvent } from 'react'

import { api, errorMessage } from '../api'
import type { Order, OrderCreateInput, Patient } from '../types'

interface OrderFormProps {
  onCreated?: (order: Order) => void
}

interface FieldErrors {
  clinicName?: string
  clinicPhone?: string
  patientName?: string
  slot?: string
}

// OrderForm maps the practice/patient/slot inputs directly onto the API's snake_case
// order body; there is no DTO translation layer.
export function OrderForm({ onCreated }: OrderFormProps) {
  const [clinicName, setClinicName] = useState('')
  const [clinicPhone, setClinicPhone] = useState('')
  const [patientName, setPatientName] = useState('')
  const [patientPhone, setPatientPhone] = useState('')
  const [birthDate, setBirthDate] = useState('')
  const [reason, setReason] = useState('')
  const [slotStart, setSlotStart] = useState('')
  const [slotEnd, setSlotEnd] = useState('')
  const [errors, setErrors] = useState<FieldErrors>({})
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  function validate(): FieldErrors {
    const found: FieldErrors = {}
    if (!clinicName.trim()) found.clinicName = 'Clinic name is required'
    if (!clinicPhone.trim()) found.clinicPhone = 'Clinic phone is required'
    if (!patientName.trim()) found.patientName = 'Patient full name is required'
    if (slotStart || slotEnd) {
      if (!slotStart || !slotEnd) {
        found.slot = 'Both preferred slot start and end are required'
      } else {
        const start = new Date(slotStart)
        const end = new Date(slotEnd)
        if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) {
          found.slot = 'Preferred slots must be valid dates'
        } else if (start.getTime() >= end.getTime()) {
          found.slot = 'Preferred slot start must be before end'
        }
      }
    }
    return found
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setSubmitError(null)
    const found = validate()
    setErrors(found)
    if (Object.keys(found).length > 0) {
      return
    }
    const patient: Patient = {
      full_name: patientName.trim(),
      phone: patientPhone.trim(),
      reason: reason.trim(),
      preferred_slots: [],
    }
    if (birthDate) patient.birth_date = birthDate
    if (slotStart && slotEnd) {
      patient.preferred_slots = [
        { start: new Date(slotStart).toISOString(), end: new Date(slotEnd).toISOString() },
      ]
    }
    const input: OrderCreateInput = {
      clinic: { name: clinicName.trim(), phone: clinicPhone.trim() },
      patient,
    }
    setSubmitting(true)
    try {
      const created = await api.createOrder(input)
      setClinicName('')
      setClinicPhone('')
      setPatientName('')
      setPatientPhone('')
      setBirthDate('')
      setReason('')
      setSlotStart('')
      setSlotEnd('')
      setErrors({})
      onCreated?.(created)
    } catch (err) {
      setSubmitError(errorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form className="order-form" onSubmit={handleSubmit} aria-label="New order">
      <h3>Practice</h3>
      <label htmlFor="clinic-name">Clinic name</label>
      <input id="clinic-name" value={clinicName} onChange={(e) => setClinicName(e.target.value)} />
      {errors.clinicName && <p className="field-error">{errors.clinicName}</p>}
      <label htmlFor="clinic-phone">Clinic phone</label>
      <input
        id="clinic-phone"
        value={clinicPhone}
        onChange={(e) => setClinicPhone(e.target.value)}
        placeholder="+431234567"
      />
      {errors.clinicPhone && <p className="field-error">{errors.clinicPhone}</p>}

      <h3>Patient</h3>
      <label htmlFor="patient-name">Patient full name</label>
      <input id="patient-name" value={patientName} onChange={(e) => setPatientName(e.target.value)} />
      {errors.patientName && <p className="field-error">{errors.patientName}</p>}
      <label htmlFor="patient-phone">Patient phone (optional)</label>
      <input id="patient-phone" value={patientPhone} onChange={(e) => setPatientPhone(e.target.value)} />
      <label htmlFor="patient-birth-date">Birth date (optional)</label>
      <input id="patient-birth-date" type="date" value={birthDate} onChange={(e) => setBirthDate(e.target.value)} />
      <label htmlFor="patient-reason">Reason (optional)</label>
      <input id="patient-reason" value={reason} onChange={(e) => setReason(e.target.value)} />

      <h3>Preferred time</h3>
      <label htmlFor="slot-start">Preferred slot start (optional)</label>
      <input id="slot-start" type="datetime-local" value={slotStart} onChange={(e) => setSlotStart(e.target.value)} />
      <label htmlFor="slot-end">Preferred slot end (optional)</label>
      <input id="slot-end" type="datetime-local" value={slotEnd} onChange={(e) => setSlotEnd(e.target.value)} />
      {errors.slot && <p className="field-error">{errors.slot}</p>}

      {submitError && (
        <p className="error" role="alert">
          {submitError}
        </p>
      )}
      <button type="submit" disabled={submitting}>
        {submitting ? 'Placing order…' : 'Place order'}
      </button>
    </form>
  )
}
