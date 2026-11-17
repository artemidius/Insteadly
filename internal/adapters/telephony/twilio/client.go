package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// APIError is a non-2xx response from the Twilio REST API. Message and MoreInfo are
// log-only: they can contain the destination number or other account details.
type APIError struct {
	StatusCode int
	Code       int
	MoreInfo   string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("twilio: HTTP %d (code %d)", e.StatusCode, e.Code)
	}
	return fmt.Sprintf("twilio: HTTP %d", e.StatusCode)
}

type callResponse struct {
	SID    string `json:"sid"`
	Status string `json:"status"`
}

// createCall starts one outbound call and returns Twilio's CallSid.
func (a *Adapter) createCall(ctx context.Context, id domain.CallID, to string) (string, error) {
	form := url.Values{}
	form.Set("To", to)
	form.Set("From", a.cfg.FromNumber)
	form.Set("Url", a.voiceURL(id))
	form.Set("StatusCallback", a.statusURL(id))
	for _, event := range []string{"initiated", "ringing", "answered", "completed"} {
		form.Add("StatusCallbackEvent", event)
	}
	// Trial accounts reject optional Calls parameters ("trial accounts have limited
	// parameter access"), so defaults (Method=POST, StatusCallbackMethod=POST,
	// Record=false) are omitted and Timeout is opt-in.
	if a.cfg.RingTimeout > 0 {
		form.Set("Timeout", strconv.Itoa(int(a.cfg.RingTimeout/time.Second)))
	}

	var out callResponse
	path := "/2010-04-01/Accounts/" + url.PathEscape(a.cfg.AccountSID) + "/Calls.json"
	if err := a.postForm(ctx, path, form, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// Phone numbers are redacted; Twilio puts the destination in some messages.
			a.log.Debug("twilio: create call rejected",
				"status", apiErr.StatusCode,
				"code", apiErr.Code,
				"more_info", apiErr.MoreInfo,
				"message", redactPhones(apiErr.Message),
			)
		}
		return "", fmt.Errorf("create call: %w", err)
	}
	if out.SID == "" {
		return "", fmt.Errorf("create call: response has no call sid")
	}
	return out.SID, nil
}

// updateCall redirects a live call to new TwiML (used for outbound DTMF).
func (a *Adapter) updateCall(ctx context.Context, callSID, twiml string) error {
	form := url.Values{}
	form.Set("Twiml", twiml)
	path := "/2010-04-01/Accounts/" + url.PathEscape(a.cfg.AccountSID) + "/Calls/" + url.PathEscape(callSID) + ".json"
	if err := a.postForm(ctx, path, form, nil); err != nil {
		return fmt.Errorf("update call: %w", err)
	}
	return nil
}

// endCall asks Twilio to hang up. The local session is already ended by the caller.
func (a *Adapter) endCall(ctx context.Context, callSID string) error {
	form := url.Values{}
	form.Set("Status", "completed")
	path := "/2010-04-01/Accounts/" + url.PathEscape(a.cfg.AccountSID) + "/Calls/" + url.PathEscape(callSID) + ".json"
	if err := a.postForm(ctx, path, form, nil); err != nil {
		return fmt.Errorf("end call: %w", err)
	}
	return nil
}

func (a *Adapter) postForm(ctx context.Context, path string, form url.Values, out any) error {
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.apiBaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(a.cfg.AccountSID, a.cfg.AuthToken)

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
		Code     int    `json:"code"`
		Message  string `json:"message"`
		MoreInfo string `json:"more_info"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		apiErr.Code = payload.Code
		apiErr.Message = payload.Message
		apiErr.MoreInfo = payload.MoreInfo
	} else {
		// Some edge/proxy errors are not JSON; keep a short, log-only hint.
		apiErr.Message = string(body)
	}
	return apiErr
}

var phoneLike = regexp.MustCompile(`\+?[0-9][0-9 ().-]{5,}[0-9]`)

// redactPhones keeps diagnostic messages useful without writing full phone numbers to
// logs. It is only used for Twilio's human-readable error text.
func redactPhones(s string) string {
	return phoneLike.ReplaceAllString(s, "[number]")
}
