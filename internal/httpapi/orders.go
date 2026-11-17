package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// maxOrderBodyBytes bounds the create payload at 1 MiB; orders are tiny.
const maxOrderBodyBytes = 1 << 20

var birthDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// createOrder decodes, validates, and stores a new order. The server owns IDs,
// timestamps, and the initial status; the API cannot mint an already-paid order.
func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOrderBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var order domain.Order
	if err := dec.Decode(&order); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}

	if order.ID != "" {
		writeError(w, http.StatusBadRequest, "bad_request", `field "id" is server-assigned and must be empty`)
		return
	}
	if order.Status != "" && order.Status != domain.OrderCreated {
		writeError(w, http.StatusBadRequest, "bad_request", `field "status" must be empty or "created"`)
		return
	}
	// Ownership and payment state are server-assigned; a client must not be able to mint
	// a paid/owned order.
	if order.UserID != "" {
		writeError(w, http.StatusBadRequest, "bad_request", `field "user_id" is server-assigned and must be empty`)
		return
	}
	if order.PaidAt != nil || order.PaymentReference != nil {
		writeError(w, http.StatusBadRequest, "bad_request", `payment fields are server-assigned and must be empty`)
		return
	}
	if a.authCookieMode() {
		user, ok := currentUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid session is required")
			return
		}
		order.UserID = user.ID
	}
	if problems := validateNewOrder(order); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, "bad_request", strings.Join(problems, "; "))
		return
	}

	stored, err := a.app.Storage.Orders().Create(r.Context(), order)
	if err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+string(stored.ID))
	writeJSON(w, http.StatusCreated, stored)
}

// listOrders returns orders, newest first; empty yields [] and never null. In cookie mode
// the list is scoped to the session user (handler-side; SQL filtering is a C7 optimization).
func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := a.app.Storage.Orders().List(r.Context())
	if err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	user, scoped := currentUser(r)
	if a.authCookieMode() && !scoped {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid session is required")
		return
	}
	if a.authCookieMode() {
		owned := make([]domain.Order, 0, len(orders))
		for _, order := range orders {
			if order.UserID != "" && order.UserID == user.ID {
				owned = append(owned, order)
			}
		}
		orders = owned
	}
	if orders == nil {
		orders = []domain.Order{}
	}
	writeJSON(w, http.StatusOK, orders)
}

// getOrder returns one order by ID; malformed IDs are indistinguishable from missing
// ones at the storage port, so both answer 404.
func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	id := domain.OrderID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "order id is required")
		return
	}
	order, err := a.app.Storage.Orders().Get(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	if !a.ownsOrder(r, order) {
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
		return
	}
	writeJSON(w, http.StatusOK, order)
}

// validateNewOrder collects every violation so the client can fix them in one round
// trip instead of one error at a time.
func validateNewOrder(o domain.Order) []string {
	var problems []string
	if strings.TrimSpace(o.Clinic.Name) == "" {
		problems = append(problems, `field "clinic.name" is required`)
	}
	if strings.TrimSpace(o.Clinic.Phone) == "" {
		problems = append(problems, `field "clinic.phone" is required`)
	}
	if strings.TrimSpace(o.Patient.FullName) == "" {
		problems = append(problems, `field "patient.full_name" is required`)
	}
	if o.Patient.BirthDate != "" && !birthDatePattern.MatchString(o.Patient.BirthDate) {
		problems = append(problems, `field "patient.birth_date" must match YYYY-MM-DD`)
	}
	for i, slot := range o.Patient.PreferredSlots {
		if !slot.Start.Before(slot.End) {
			problems = append(problems, fmt.Sprintf(`field "patient.preferred_slots[%d]" must have start before end`, i))
		}
	}
	return problems
}
