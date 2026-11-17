// Package mistral implements ports.SpeechToText with Mistral's realtime (WebSocket) and
// batch (multipart REST) transcription endpoints. Provider concepts (base64 audio, WAV
// containers, model names, SSE/WS events) stay inside this package.
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

// ErrStreamClosed is returned when feeding audio to a stream that has been closed.
var ErrStreamClosed = errors.New("mistral stt: stream closed")

// APIError is a non-2xx Mistral REST response. It deliberately retains no message or raw
// body, which may echo medical text or secrets.
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
		return fmt.Sprintf("mistral stt: HTTP %d (%s)", e.StatusCode, strings.Join(parts, " "))
	}
	return fmt.Sprintf("mistral stt: HTTP %d", e.StatusCode)
}

// Temporary reports a provider-side failure (429 or any 5xx). It is classification only,
// never permission to retry.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || (e.StatusCode >= 500 && e.StatusCode <= 599)
}

// RetryDelay returns the parsed Retry-After, or 0 when absent/unparseable.
func (e *APIError) RetryDelay() time.Duration { return e.RetryAfter }

// ProtocolError is a WebSocket-level protocol failure (bad event, wrong handshake,
// unexpected close). It carries only a stable code, never provider text.
type ProtocolError struct {
	Code string
}

func (e *ProtocolError) Error() string { return "mistral stt: protocol error: " + e.Code }

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

// normalizeCode accepts Mistral's `code` field as either a JSON string or number.
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

// parseRetryAfter reads integer seconds; an unparseable value yields 0 (no delay).
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
