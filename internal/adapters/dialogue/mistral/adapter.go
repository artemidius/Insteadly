package mistral

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	defaultBaseURL      = "https://api.mistral.ai"
	defaultModel        = "mistral-small-2603"
	chatCompletionsPath = "/v1/chat/completions"

	// Adapter constants, not env surface (§3.6).
	temperature      = 0.1
	maxTokens        = 512
	reservedToolName = "finish_call"
)

// Config is the adapter's own settings.
type Config struct {
	APIKey     string
	BaseURL    string
	Model      string
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Adapter implements ports.Dialogue with Mistral Chat Completions.
type Adapter struct {
	cfg        Config
	baseURL    string
	httpClient *http.Client
	log        *slog.Logger
}

var _ ports.Dialogue = (*Adapter)(nil)

// New validates the adapter configuration and performs no network I/O.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("mistral dialogue: api key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newDefaultHTTPClient()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Adapter{cfg: cfg, baseURL: base, httpClient: cfg.HTTPClient, log: log}, nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil {
		return "", errors.New("mistral dialogue: invalid base url")
	}
	if u.Host == "" {
		return "", errors.New("mistral dialogue: base url must be an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("mistral dialogue: base url must be http(s)")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("mistral dialogue: non-loopback http base url would expose the api key")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func newDefaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// The built-in prompt is short, German-first medical-booking instruction. A non-empty
// SystemPrompt replaces it, but the fixed protocol sentence is always appended.
const defaultSystemPrompt = `Du bist Insteadly, ein KI-Assistent, der einen Arzttermin für einen Patienten vereinbart. Nenne dich als KI-Assistent, beschreibe den Zweck des Anrufs, sei knapp, erfinde keine Patientendaten oder bestätigte Termine und nutze die bereitgestellten Werkzeuge für jede Änderung.`

const protocolSentence = ` Beende den Anruf immer mit genau einem Aufruf der Funktion finish_call mit einem vollständigen, gültigen Ergebnis.`

// NewSession validates the request (no HTTP call) and returns a session. Caller-owned byte
// slices (tool Parameters, OutcomeSchema) are deep-copied before use so later mutation of
// the caller's slices cannot change requests made by this session.
func (a *Adapter) NewSession(ctx context.Context, req ports.SessionRequest) (ports.DialogueSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(req.OrderID)) == "" {
		return nil, errors.New("mistral dialogue: order id is required")
	}

	outcomeSchema := cloneBytes(req.OutcomeSchema)
	reqTools := copyTools(req.Tools)
	if !isJSONObjectSchema(outcomeSchema) {
		return nil, errors.New("mistral dialogue: outcome schema must be a JSON object schema")
	}
	prompt := strings.TrimSpace(req.SystemPrompt)
	if prompt == "" {
		prompt = defaultSystemPrompt
	}
	prompt += protocolSentence

	tools, err := translateTools(reqTools, outcomeSchema)
	if err != nil {
		return nil, err
	}

	sessCtx, sessCancel := context.WithCancel(context.Background())
	s := &session{
		adapter:    a,
		orderID:    req.OrderID,
		prompt:     prompt,
		tools:      tools,
		messages:   []chatMessage{{Role: "system", Content: prompt}},
		baseCtx:    sessCtx,
		baseCancel: sessCancel,
	}
	return s, nil
}

// copyTools deep-copies the caller's tool slice, including each Parameters byte slice.
func copyTools(in []domain.Tool) []domain.Tool {
	out := make([]domain.Tool, len(in))
	for i, t := range in {
		out[i] = t
		out[i].Parameters = cloneBytes(t.Parameters)
	}
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// --- tool validation error helpers (name the tool, never the schema contents) ---

func errInvalidToolName(name string) error {
	return fmt.Errorf("mistral dialogue: invalid tool name %q", name)
}

var errReservedToolName = errors.New("mistral dialogue: finish_call is reserved")

func errDuplicateToolName(name string) error {
	return fmt.Errorf("mistral dialogue: duplicate tool name %q", name)
}

func errInvalidToolSchema(name string) error {
	return fmt.Errorf("mistral dialogue: tool %q has invalid parameters schema", name)
}
