// Package twilio implements ports.Telephony with Twilio Programmable Voice and Media
// Streams. Provider concepts (CallSid, µ-law, TwiML, webhook signatures) stay inside this
// package; the core sees only ports.CallSession and canonical audio.
package twilio

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	defaultAPIBaseURL  = "https://api.twilio.com"
	defaultHTTPTimeout = 20 * time.Second
)

var accountSIDPattern = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)

// Config is the adapter's own settings. internal/app maps config.Twilio into it; the
// adapter never imports internal/core/config.
type Config struct {
	AccountSID    string
	AuthToken     string // secret; never logged
	FromNumber    string
	PublicBaseURL string        // https origin Twilio can reach, no trailing slash
	RingTimeout   time.Duration // 5s..10m, default 30s
	APIBaseURL    string        // default https://api.twilio.com (tests override)
	HTTPClient    *http.Client  // default 20s timeout
	Logger        *slog.Logger  // nil = discard
}

// Adapter dials through Twilio and owns the webhook/WebSocket surface for its calls.
type Adapter struct {
	cfg           Config
	publicBaseURL string
	apiBaseURL    string
	httpClient    *http.Client
	log           *slog.Logger
	handler       http.Handler

	mu       sync.Mutex
	sessions map[domain.CallID]*session
}

var _ ports.Telephony = (*Adapter)(nil)

// New validates the adapter configuration and builds its HTTP handler.
func New(cfg Config) (*Adapter, error) {
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if !accountSIDPattern.MatchString(cfg.AccountSID) {
		return nil, fmt.Errorf("twilio: account sid must look like AC followed by 32 hex characters")
	}
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return nil, fmt.Errorf("twilio: auth token is required")
	}
	if !e164Pattern.MatchString(cfg.FromNumber) {
		return nil, fmt.Errorf("twilio: caller id must be E.164")
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("twilio: public base URL must be an absolute http(s) URL")
	}
	// Twilio itself requires https. http is accepted only so tests can point the adapter
	// at an httptest server; production config validation rejects it.
	if cfg.RingTimeout != 0 && (cfg.RingTimeout < 5*time.Second || cfg.RingTimeout > 10*time.Minute) {
		return nil, fmt.Errorf("twilio: ring timeout must be 0 (Twilio default) or between 5s and 10m")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultAPIBaseURL
	}
	apiBase, err := url.Parse(strings.TrimRight(cfg.APIBaseURL, "/"))
	if err != nil || apiBase.Host == "" || (apiBase.Scheme != "https" && apiBase.Scheme != "http") {
		return nil, fmt.Errorf("twilio: invalid API base URL")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	a := &Adapter{
		cfg:           cfg,
		publicBaseURL: cfg.PublicBaseURL,
		apiBaseURL:    apiBase.String(),
		httpClient:    cfg.HTTPClient,
		log:           log,
		sessions:      make(map[domain.CallID]*session),
	}
	a.handler = a.newHandler()
	return a, nil
}

// Handler returns the Twilio-facing HTTP routes. The composition root mounts it under
// /webhooks/telephony/twilio/; nothing in the core imports it.
func (a *Adapter) Handler() http.Handler {
	return a.handler
}

// Dial starts one outbound call. It returns as soon as Twilio has accepted the call;
// progress arrives through status callbacks and the Media Streams WebSocket.
func (a *Adapter) Dial(ctx context.Context, order domain.Order) (ports.CallSession, error) {
	phone := strings.TrimSpace(order.Clinic.Phone)
	if !e164Pattern.MatchString(phone) {
		return nil, fmt.Errorf("twilio: order %s: clinic phone must be E.164", order.ID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := domain.NewCallID()
	s := newSession(a, id)
	a.add(s)

	sid, err := a.createCall(ctx, id, phone)
	if err != nil {
		s.finish(domain.EndError)
		// The call never existed; no late callback needs the tombstone.
		a.remove(s.id)
		return nil, fmt.Errorf("twilio: dial order %s: %w", order.ID, err)
	}
	s.setProviderSID(sid)
	s.progress(domain.EventDialing)
	return s, nil
}

func (a *Adapter) add(s *session) {
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()
}

func (a *Adapter) remove(id domain.CallID) {
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
}

func (a *Adapter) lookup(id domain.CallID) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}
