package httpapi

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const (
	maxIdempotencyKey = 128
	defaultIdemTTL    = 10 * time.Minute
)

// idempotencyCache remembers the order returned for one (order, key) pair. It is
// deliberately in-process and prototype-scoped: a durable payment-idempotency store is
// C6 work. Entries expire lazily on access.
type idempotencyCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]idempotencyEntry
	now     func() time.Time
}

type idempotencyEntry struct {
	order   domain.Order
	expires time.Time
}

func newIdempotencyCache(ttl time.Duration) *idempotencyCache {
	if ttl <= 0 {
		ttl = defaultIdemTTL
	}
	return &idempotencyCache{
		ttl:     ttl,
		entries: make(map[string]idempotencyEntry),
		now:     time.Now,
	}
}

func (c *idempotencyCache) get(key string) (domain.Order, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	entry, ok := c.entries[key]
	if !ok {
		return domain.Order{}, false
	}
	return entry.order, true
}

func (c *idempotencyCache) put(key string, order domain.Order) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	c.entries[key] = idempotencyEntry{order: order, expires: c.now().Add(c.ttl)}
}

func (c *idempotencyCache) pruneLocked() {
	now := c.now()
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
}

// apiKeyAuth gates the place-call endpoint with the single configured API key. An empty
// key disables auth (development only; C6 owns real user auth). The comparison is
// constant-time over the token bytes.
func (a *API) apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	if a.cfg.API.Key == "" {
		return next
	}
	want := []byte(a.cfg.API.Key)
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(token), want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
			return
		}
		next(w, r)
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// callOrder makes an existing created order eligible to dial now. It is intentionally an
// eligibility endpoint: it drives created → paid → scheduled(next_attempt_at = now) and
// returns 202; the scheduler claims and runs it on the next tick. There is no
// synchronous dial path.
func (a *API) callOrder(w http.ResponseWriter, r *http.Request) {
	id := domain.OrderID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "order id is required")
		return
	}
	key, ok := requestIdempotencyKey(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("Idempotency-Key must be at most %d characters", maxIdempotencyKey))
		return
	}
	cacheKey := string(id) + "\x00" + key
	if key != "" {
		if stored, hit := a.idem.get(cacheKey); hit {
			w.Header().Set("Location", "/api/v1/orders/"+string(stored.ID))
			writeJSON(w, http.StatusOK, stored)
			return
		}
	}

	order, err := a.app.Storage.Orders().Get(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	if code, msg, terminal := callConflict(order); terminal {
		writeError(w, http.StatusConflict, code, msg)
		return
	}

	now := time.Now()
	scheduled, err := a.app.Storage.Orders().Update(r.Context(), id, func(o *domain.Order) error {
		if o.Status != domain.OrderCreated {
			return fmt.Errorf("%w: order is %q", domain.ErrInvalidTransition, o.Status)
		}
		// created → paid is the C6 payment-gate seam; C5 has no paid endpoint, so this
		// is the only path to scheduled besides the scheduler's own paid handling.
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Schedule(&now)
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			// Another actor changed the order between Get and Update. Answer with the
			// authoritative state instead of pretending the call was scheduled.
			current, getErr := a.app.Storage.Orders().Get(r.Context(), id)
			if getErr != nil {
				a.writeStoreError(w, r, getErr)
				return
			}
			code, msg, _ := callConflict(current)
			if code == "" {
				code, msg = "conflict", "order changed while it was being scheduled"
			}
			writeError(w, http.StatusConflict, code, msg)
			return
		}
		a.writeStoreError(w, r, err)
		return
	}
	if key != "" {
		a.idem.put(cacheKey, scheduled)
	}
	w.Header().Set("Location", "/api/v1/orders/"+string(scheduled.ID))
	writeJSON(w, http.StatusAccepted, scheduled)
}

// callConflict classifies a non-created order. terminal is true when the order cannot be
// scheduled by this request.
func callConflict(order domain.Order) (code, message string, terminal bool) {
	switch order.Status {
	case domain.OrderPaid, domain.OrderScheduled:
		return "already_scheduled", "order is already scheduled", true
	case domain.OrderCreated:
		return "", "", false
	default:
		return "conflict", fmt.Sprintf("order is %q and can no longer be scheduled", order.Status), true
	}
}

func requestIdempotencyKey(r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", true
	}
	if len(key) > maxIdempotencyKey || strings.ContainsRune(key, 0) {
		return "", false
	}
	return key, true
}
