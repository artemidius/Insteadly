// Package telnyx implements ports.Telephony with Telnyx Call Control and bidirectional
// Media Streams. Provider concepts (call_control_id, connection_id, stream_id, µ-law,
// webhook signatures) stay inside this package; the core sees only ports.CallSession and
// canonical PCM16.
//
// The adapter is the C7 swapability proof: it is selected by
// INSTEADLY_TELEPHONY_PROVIDER=telnyx and exercised entirely against in-repo fake HTTP
// and WebSocket servers. It has no Telnyx account or credential dependency in tests.
package telnyx

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
	defaultAPIBaseURL = "https://api.telnyx.com"
	// Media Streams/Webhooks are versioned under /v2; the API base URL is the origin.
	apiVersion = "/v2"
	// Webhook paths are adapter-owned transport details; the core never sees them.
	webhookPrefix = "/webhooks/telephony/telnyx"
	webhookPath   = webhookPrefix + "/webhook/"
	streamPath    = webhookPrefix + "/stream/"

	defaultHTTPTimeout = 20 * time.Second

	// Telnyx docs: outbound media is requested with stream_bidirectional_mode=rtp and a
	// PCMU codec, which matches the canonical µ-law/8 kHz stream the port expects.
	streamTrack      = "both_tracks"
	bidirectionalMod = "rtp"
	bidirectionalCD  = "PCMU"
)

var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// Config is the adapter's own settings. internal/app maps config.Telnyx into it; the
// adapter never imports internal/core/config.
type Config struct {
	APIKey           string // secret; never logged
	ConnectionID     string // Call Control application id
	FromNumber       string
	PublicBaseURL    string        // https origin Telnyx can reach, no trailing slash
	WebhookPublicKey string        // optional Ed25519 public key (base64 or PEM)
	RingTimeout      time.Duration // 0 = Telnyx default (30 s), else 5s..10m
	APIBaseURL       string        // default https://api.telnyx.com (tests override)
	HTTPClient       *http.Client  // default 20 s timeout
	Logger           *slog.Logger  // nil = discard
}

// Adapter dials through Telnyx and owns the webhook/WebSocket surface for its calls.
type Adapter struct {
	cfg           Config
	publicBaseURL string
	apiBaseURL    string
	httpClient    *http.Client
	log           *slog.Logger
	handler       http.Handler
	publicKey     ed25519PublicKey // nil when no WebhookPublicKey is configured

	mu       sync.Mutex
	sessions map[domain.CallID]*session
}

var _ ports.Telephony = (*Adapter)(nil)

// New validates the adapter configuration and builds its HTTP handler. It parses the
// configured signature key eagerly so a malformed key fails at startup, not on the first
// signed webhook.
func New(cfg Config) (*Adapter, error) {
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("telnyx: api key is required")
	}
	if strings.TrimSpace(cfg.ConnectionID) == "" {
		return nil, fmt.Errorf("telnyx: connection id is required")
	}
	if !e164Pattern.MatchString(cfg.FromNumber) {
		return nil, fmt.Errorf("telnyx: caller id must be E.164")
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("telnyx: public base URL must be an absolute http(s) URL")
	}
	// Telnyx itself requires https. http is accepted only so tests can point the adapter
	// at an httptest server; production config validation rejects it.
	if cfg.RingTimeout != 0 && (cfg.RingTimeout < 5*time.Second || cfg.RingTimeout > 10*time.Minute) {
		return nil, fmt.Errorf("telnyx: ring timeout must be 0 (Telnyx default) or between 5s and 10m")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultAPIBaseURL
	}
	apiBase, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/"))
	if err != nil || apiBase.Host == "" || (apiBase.Scheme != "https" && apiBase.Scheme != "http") {
		return nil, fmt.Errorf("telnyx: invalid API base URL")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var pub ed25519PublicKey
	if strings.TrimSpace(cfg.WebhookPublicKey) != "" {
		pub, err = parseEd25519PublicKey(cfg.WebhookPublicKey)
		if err != nil {
			return nil, fmt.Errorf("telnyx: invalid webhook public key: %w", err)
		}
	}

	a := &Adapter{
		cfg:           cfg,
		publicBaseURL: cfg.PublicBaseURL,
		apiBaseURL:    apiBase.String(),
		httpClient:    cfg.HTTPClient,
		log:           log,
		publicKey:     pub,
		sessions:      make(map[domain.CallID]*session),
	}
	a.handler = a.newHandler()
	return a, nil
}

// Handler returns the Telnyx-facing HTTP routes. The composition root mounts it under
// /webhooks/telephony/telnyx/; nothing in the core imports it.
func (a *Adapter) Handler() http.Handler {
	return a.handler
}

// Dial starts one outbound call. It returns as soon as Telnyx has accepted the call;
// progress arrives through call-control webhooks and the Media Streams WebSocket (which
// is attached on call.answered via streaming_start).
func (a *Adapter) Dial(ctx context.Context, order domain.Order) (ports.CallSession, error) {
	phone := strings.TrimSpace(order.Clinic.Phone)
	if !e164Pattern.MatchString(phone) {
		return nil, fmt.Errorf("telnyx: order %s: clinic phone must be E.164", order.ID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := domain.NewCallID()
	s := newSession(a, id)
	a.add(s)

	ccid, err := a.createCall(ctx, id, phone)
	if err != nil {
		s.finish(domain.EndError)
		// The call never existed; no late callback needs the tombstone.
		a.remove(s.id)
		return nil, fmt.Errorf("telnyx: dial order %s: %w", order.ID, err)
	}
	s.setProviderSID(ccid)
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

func (a *Adapter) absoluteURL(path string) string {
	return a.publicBaseURL + path
}

func (a *Adapter) webhookURL(id domain.CallID) string {
	return a.absoluteURL(webhookPath + string(id))
}

// streamURL derives the WSS endpoint from the public https origin. Telnyx accepts the
// stream URL in streaming_start (unlike TwiML, a query string is allowed, but none is
// needed).
func (a *Adapter) streamURL(id domain.CallID) string {
	u, err := url.Parse(a.publicBaseURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = streamPath + string(id)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
