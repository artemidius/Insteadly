package domain

import "time"

// Notification is one outbound message to a patient (ports declared in C0, adapter in C7).
type Notification struct {
	OrderID OrderID
	Channel string // "email" | "sms"
	To      string
	Subject string
	Body    string
}

// PaymentEvent is the normalized result of a payment provider webhook (adapter in C6).
type PaymentEvent struct {
	OrderID  OrderID
	Paid     bool
	Refunded bool
	At       time.Time
}
