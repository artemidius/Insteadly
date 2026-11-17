// Package openai implements ports.Dialogue with OpenAI Chat Completions (SSE). Tool
// definitions, model names, SSE frames, and the reserved finish_call outcome stay inside
// this package; the core sees only streamed text, tool calls, and a validated Outcome.
package openai

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

// Adapter-local turn/session sentinels.
var (
	// ErrTurnInProgress is returned when a session already has an in-flight turn.
	ErrTurnInProgress = errors.New("openai dialogue: turn in progress")
	// ErrSessionFinished is returned after a terminal finish_call (or after Close).
	ErrSessionFinished = errors.New("openai dialogue: session finished")
	// ErrTurnTruncated is returned when the model ends a turn with an incomplete result.
	ErrTurnTruncated = errors.New("openai dialogue: turn truncated")
)

// APIError is a non-2xx OpenAI REST response. It retains no message or raw body.
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
		return fmt.Sprintf("openai dialogue: HTTP %d (%s)", e.StatusCode, strings.Join(parts, " "))
	}
	return fmt.Sprintf("openai dialogue: HTTP %d", e.StatusCode)
}

// Temporary reports a provider-side failure (429 or any 5xx); classification only.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || (e.StatusCode >= 500 && e.StatusCode <= 599)
}

// RetryDelay returns the parsed Retry-After, or 0.
func (e *APIError) RetryDelay() time.Duration { return e.RetryAfter }

// decodeAPIError parses a bounded OpenAI error body without retaining message text.
func decodeAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		Error struct {
			Type  string          `json:"type"`
			Code  json.RawMessage `json:"code"`
			Param string          `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		apiErr.Type = payload.Error.Type
		apiErr.Code = normalizeCode(payload.Error.Code)
		apiErr.Param = payload.Error.Param
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
