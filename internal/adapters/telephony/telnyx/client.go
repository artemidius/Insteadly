package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// APIError is a non-2xx response from the Telnyx REST API. Title/Detail are log-only:
// they can contain the destination number or other account details.
type APIError struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("telnyx: HTTP %d (code %s)", e.StatusCode, e.Code)
	}
	return fmt.Sprintf("telnyx: HTTP %d", e.StatusCode)
}

// dialRequest is the verified POST /v2/calls body (openapi spec3, CallRequest).
type dialRequest struct {
	To           string `json:"to"`
	From         string `json:"from"`
	ConnectionID string `json:"connection_id"`
	WebhookURL   string `json:"webhook_url"`
	TimeoutSecs  int    `json:"timeout_secs,omitempty"`
	ClientState  string `json:"client_state,omitempty"`
}

type callResponse struct {
	Data struct {
		CallControlID string `json:"call_control_id"`
		CallLegID     string `json:"call_leg_id"`
		CallSessionID string `json:"call_session_id"`
		IsAlive       bool   `json:"is_alive"`
	} `json:"data"`
}

// streamingStartRequest is the verified POST .../actions/streaming_start body. The
// bidirectional fields are what make outbound media (rtp/PCMU) possible at all; the
// Telnyx default is an mp3 one-way stream.
type streamingStartRequest struct {
	StreamURL                string `json:"stream_url"`
	StreamTrack              string `json:"stream_track"`
	StreamBidirectionalMode  string `json:"stream_bidirectional_mode"`
	StreamBidirectionalCodec string `json:"stream_bidirectional_codec"`
}

type sendDTMFRequest struct {
	Digits string `json:"digits"`
}

// createCall starts one outbound call and returns Telnyx's call_control_id.
func (a *Adapter) createCall(ctx context.Context, id domain.CallID, to string) (string, error) {
	body := dialRequest{
		To:           to,
		From:         a.cfg.FromNumber,
		ConnectionID: a.cfg.ConnectionID,
		WebhookURL:   a.webhookURL(id),
	}
	if a.cfg.RingTimeout > 0 {
		body.TimeoutSecs = int(a.cfg.RingTimeout / time.Second)
	}
	var out callResponse
	if err := a.postJSON(ctx, "/calls", body, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// Phone numbers are redacted; Telnyx puts the destination in some messages.
			a.log.Debug("telnyx: create call rejected",
				"status", apiErr.StatusCode,
				"code", apiErr.Code,
				"title", apiErr.Title,
				"detail", redactPhones(apiErr.Detail),
			)
		}
		return "", fmt.Errorf("create call: %w", err)
	}
	if out.Data.CallControlID == "" {
		return "", fmt.Errorf("create call: response has no call_control_id")
	}
	return out.Data.CallControlID, nil
}

// startStreaming attaches the bidirectional media stream to an answered call.
func (a *Adapter) startStreaming(ctx context.Context, callControlID string, id domain.CallID) error {
	body := streamingStartRequest{
		StreamURL:                a.streamURL(id),
		StreamTrack:              streamTrack,
		StreamBidirectionalMode:  bidirectionalMod,
		StreamBidirectionalCodec: bidirectionalCD,
	}
	if err := a.postJSON(ctx, "/calls/"+callControlID+"/actions/streaming_start", body, nil); err != nil {
		return fmt.Errorf("streaming start: %w", err)
	}
	return nil
}

// sendDTMF uses the documented Call Control send-DTMF action. The media-stream socket
// itself has no outbound DTMF message.
func (a *Adapter) sendDTMF(ctx context.Context, callControlID, digits string) error {
	if err := a.postJSON(ctx, "/calls/"+callControlID+"/actions/send_dtmf", sendDTMFRequest{Digits: digits}, nil); err != nil {
		return fmt.Errorf("send dtmf: %w", err)
	}
	return nil
}

// endCall asks Telnyx to hang up. The local session is already ended by the caller.
func (a *Adapter) endCall(ctx context.Context, callControlID string) error {
	err := a.postJSON(ctx, "/calls/"+callControlID+"/actions/hangup", struct{}{}, nil)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == alreadyEndedCode {
		return nil // already ended: the intended state
	}
	return fmt.Errorf("end call: %w", err)
}

// postJSON sends one authenticated JSON request. out may be nil.
func (a *Adapter) postJSON(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+apiVersion+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		Errors []struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && len(payload.Errors) > 0 {
		first := payload.Errors[0]
		apiErr.Code = first.Code
		apiErr.Title = first.Title
		apiErr.Detail = first.Detail
	}
	return apiErr
}

// redactPhones keeps diagnostic messages useful without writing full phone numbers to
// logs. It is only used for Telnyx's human-readable error text.
func redactPhones(s string) string {
	return phoneLike.ReplaceAllString(s, "[number]")
}

var phoneLike = regexp.MustCompile(`\+?[0-9][0-9 ().-]{5,}[0-9]`)
