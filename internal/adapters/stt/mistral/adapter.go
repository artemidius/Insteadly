package mistral

import (
	"context"
	"errors"
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
	defaultBaseURL          = "https://api.mistral.ai"
	defaultRealtimeModel    = "voxtral-mini-transcribe-realtime-3"
	defaultBatchModel       = "voxtral-mini-transcribe-3"
	defaultStreamingDelayMS = 480
	realtimeTranscriptPath  = "/v1/audio/transcriptions/realtime"
	batchTranscriptPath     = "/v1/audio/transcriptions"
	handshakeTimeout        = 10 * time.Second
	writeTimeout            = 5 * time.Second
	maxSTTWebSocketMessage  = 1 << 20 // 1 MiB
)

// Config is the adapter's own settings. internal/app maps config.Mistral into it; the
// adapter never imports internal/core/config.
type Config struct {
	APIKey           string
	BaseURL          string
	RealtimeModel    string
	BatchModel       string
	StreamingDelayMS int
	HTTPClient       *http.Client
	Logger           *slog.Logger
}

// Adapter implements ports.SpeechToText with Mistral transcription.
type Adapter struct {
	cfg        Config
	baseURL    string
	httpClient *http.Client
	log        *slog.Logger
}

var _ ports.SpeechToText = (*Adapter)(nil)

// New validates the adapter configuration and performs no network I/O.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("mistral stt: api key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.RealtimeModel == "" {
		cfg.RealtimeModel = defaultRealtimeModel
	}
	if cfg.BatchModel == "" {
		cfg.BatchModel = defaultBatchModel
	}
	if cfg.StreamingDelayMS == 0 {
		cfg.StreamingDelayMS = defaultStreamingDelayMS
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

// normalizeBaseURL trims trailing slashes and enforces https (or loopback http for tests).
func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil {
		return "", errors.New("mistral stt: invalid base url")
	}
	if u.Host == "" {
		return "", errors.New("mistral stt: base url must be an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("mistral stt: base url must be http(s)")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("mistral stt: non-loopback http base url would expose the api key")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// newDefaultHTTPClient uses sane transport timeouts without a whole-client Timeout that
// would kill the long-lived WebSocket upgrade.
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

// realtimeURL is the WebSocket endpoint with only the documented model query.
func (a *Adapter) realtimeURL() string {
	u, err := url.Parse(a.baseURL)
	if err != nil {
		return a.baseURL + realtimeTranscriptPath
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = realtimeTranscriptPath
	q := u.Query()
	q.Set("model", a.cfg.RealtimeModel)
	u.RawQuery = q.Encode()
	return u.String()
}

// authHeader returns the Bearer header for Mistral requests.
func (a *Adapter) authHeader() http.Header {
	return http.Header{"Authorization": []string{"Bearer " + a.cfg.APIKey}}
}

// TranscribeStream opens a live streaming transcription session.
func (a *Adapter) TranscribeStream(ctx context.Context, opts ports.StreamOptions) (ports.TranscriptionStream, error) {
	return a.transcribeStream(ctx, opts)
}

// TranscribeFile transcribes a complete headerless canonical PCM16 recording.
func (a *Adapter) TranscribeFile(ctx context.Context, r io.Reader, opts ports.BatchOptions) (domain.Transcript, error) {
	return a.transcribeFile(ctx, r, opts)
}
