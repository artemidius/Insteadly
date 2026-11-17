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

	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	defaultBaseURL     = "https://api.mistral.ai"
	defaultModel       = "voxtral-mini-tts-3-0"
	defaultVoice       = "voice_447"
	defaultMode        = "realtime"
	realtimeSpeechPath = "/v1/audio/speech/realtime"
	offlineSpeechPath  = "/v1/audio/speech"
	handshakeTimeout   = 10 * time.Second
	writeTimeout       = 5 * time.Second
	maxTTSWebSocketMsg = 4 << 20 // 4 MiB (audio deltas)
)

// Config is the adapter's own settings.
type Config struct {
	APIKey       string
	BaseURL      string
	Model        string
	DefaultVoice string
	Mode         string // "realtime" | "offline"
	HTTPClient   *http.Client
	Logger       *slog.Logger
}

// Adapter implements ports.TextToSpeech with Mistral synthesis.
type Adapter struct {
	cfg        Config
	baseURL    string
	httpClient *http.Client
	log        *slog.Logger
}

var _ ports.TextToSpeech = (*Adapter)(nil)

// New validates the adapter configuration and performs no network I/O.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("mistral tts: api key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.DefaultVoice == "" {
		cfg.DefaultVoice = defaultVoice
	}
	if cfg.Mode == "" {
		cfg.Mode = defaultMode
	}
	if cfg.Mode != "realtime" && cfg.Mode != "offline" {
		return nil, errors.New("mistral tts: mode must be realtime or offline")
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
		return "", errors.New("mistral tts: invalid base url")
	}
	if u.Host == "" {
		return "", errors.New("mistral tts: base url must be an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("mistral tts: base url must be http(s)")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("mistral tts: non-loopback http base url would expose the api key")
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

func (a *Adapter) realtimeURL() string {
	u, err := url.Parse(a.baseURL)
	if err != nil {
		return a.baseURL + realtimeSpeechPath
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = realtimeSpeechPath
	q := u.Query()
	q.Set("model", a.cfg.Model)
	u.RawQuery = q.Encode()
	return u.String()
}

func (a *Adapter) authHeader() http.Header {
	return http.Header{"Authorization": []string{"Bearer " + a.cfg.APIKey}}
}

// Speak opens a synthesis session. realtime dials the WebSocket immediately; offline only
// constructs local state and performs no network I/O until Close.
func (a *Adapter) Speak(ctx context.Context, opts ports.SpeakOptions) (ports.SpeechStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch a.cfg.Mode {
	case "offline":
		voice := opts.Voice
		if voice == "" {
			voice = a.cfg.DefaultVoice
		}
		return newOfflineStream(a, voice), nil
	default:
		return a.speakRealtime(ctx, opts)
	}
}
