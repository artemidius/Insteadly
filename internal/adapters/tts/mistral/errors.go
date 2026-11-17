// Package mistral implements ports.TextToSpeech with Mistral's realtime (WebSocket) and
// offline (REST/SSE) speech synthesis. Mistral's float32 mono 24 kHz output is converted to
// canonical PCM16 mono 16 kHz inside this package; no Mistral format crosses the port.
package mistral

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrStreamClosed is returned when feeding text to a stream that has been closed.
var ErrStreamClosed = errors.New("mistral tts: stream closed")

// APIError is a non-2xx Mistral REST response. It retains no message or raw body.
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Param      string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var parts []string
	if e.Type != "" {
		parts = append(parts, "type="+e.Type)
	}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.Param != "" {
		parts = append(parts, "param="+e.Param)
	}
	if len(parts) > 0 {
		return fmt.Sprintf("mistral tts: HTTP %d (%s)", e.StatusCode, strings.Join(parts, " "))
	}
	return fmt.Sprintf("mistral tts: HTTP %d", e.StatusCode)
}

// Temporary reports a provider-side failure (429 or any 5xx); classification only.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || (e.StatusCode >= 500 && e.StatusCode <= 599)
}

// RetryDelay returns the parsed Retry-After, or 0.
func (e *APIError) RetryDelay() time.Duration { return e.RetryAfter }

// ProtocolError is a WebSocket-level protocol failure carrying only a stable code.
type ProtocolError struct {
	Code string
}

func (e *ProtocolError) Error() string { return "mistral tts: protocol error: " + e.Code }

// decodeAPIError parses a bounded Mistral error body without retaining message text.
func decodeAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		Type  string          `json:"type"`
		Code  json.RawMessage `json:"code"`
		Param string          `json:"param"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		apiErr.Type = payload.Type
		apiErr.Code = normalizeCode(payload.Code)
		apiErr.Param = payload.Param
	}
	apiErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return apiErr
}

func normalizeCode(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
		return ""
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return s
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}
