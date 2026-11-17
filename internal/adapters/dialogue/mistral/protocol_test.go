package mistral

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestTranslateTools(t *testing.T) {
	tools := []domain.Tool{captureInfoTool()}
	out, err := translateTools(tools, outcomeSchema())
	if err != nil {
		t.Fatalf("translateTools: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("tools = %d, want 2 (caller + finish_call)", len(out))
	}
	if out[0].Type != "function" || out[0].Function.Name != "capture_info" {
		t.Errorf("caller tool = %+v", out[0])
	}
	// parameters must be the raw caller schema bytes, verbatim.
	var params map[string]any
	if err := json.Unmarshal(out[0].Function.Parameters, &params); err != nil || params["type"] != "object" {
		t.Errorf("parameters = %s", out[0].Function.Parameters)
	}
	if out[1].Function.Name != "finish_call" {
		t.Errorf("reserved tool = %+v", out[1].Function)
	}
	if string(out[1].Function.Parameters) != string(outcomeSchema()) {
		t.Error("finish_call parameters must be the outcome schema")
	}
}

func TestTranslateToolsRejects(t *testing.T) {
	cases := []struct {
		name string
		tool domain.Tool
	}{
		{"uppercase", domain.Tool{Name: "Capture", Parameters: []byte(`{"type":"object"}`)}},
		{"hyphen", domain.Tool{Name: "capture-info", Parameters: []byte(`{"type":"object"}`)}},
		{"reserved", domain.Tool{Name: "finish_call", Parameters: []byte(`{"type":"object"}`)}},
		{"non-object schema", domain.Tool{Name: "a", Parameters: []byte(`{"type":"string"}`)}},
		{"malformed schema", domain.Tool{Name: "a", Parameters: []byte(`not-json`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := translateTools([]domain.Tool{tc.tool}, outcomeSchema()); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	// Duplicate names.
	dup := []domain.Tool{captureInfoTool(), captureInfoTool()}
	if _, err := translateTools(dup, outcomeSchema()); err == nil {
		t.Fatal("duplicate names must error")
	}
}

func TestIsJSONObjectSchema(t *testing.T) {
	if !isJSONObjectSchema([]byte(`{"type":"object","properties":{}}`)) {
		t.Error("object schema rejected")
	}
	for _, bad := range []string{`{"type":"string"}`, `{}`, `[]`, `not-json`} {
		if isJSONObjectSchema([]byte(bad)) {
			t.Errorf("accepted bad schema %s", bad)
		}
	}
}

func TestDecodeContent(t *testing.T) {
	if got, _ := decodeContent([]byte(`"hello"`)); got != "hello" {
		t.Errorf("string content = %q", got)
	}
	parts := `[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"other","text":"x"}]`
	if got, _ := decodeContent([]byte(parts)); got != "ab" {
		t.Errorf("content parts = %q", got)
	}
	if got, _ := decodeContent([]byte(`null`)); got != "" {
		t.Errorf("null content = %q", got)
	}
	if got, _ := decodeContent(nil); got != "" {
		t.Errorf("empty content = %q", got)
	}
	if _, err := decodeContent([]byte(`{"unexpected":1}`)); err == nil {
		t.Error("malformed content must error")
	}
}

func TestAPIErrorParse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"object":"error","type":"rate_limit_error","code":"rate_limited","param":"model","message":"sensitive"}`))
	}))
	t.Cleanup(ts.Close)
	a := newTestAdapter(t, ts.URL)
	_, _, err := a.chat(t.Context(), []chatMessage{}, []chatTool{}, newTurn())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != 429 || apiErr.Code != "rate_limited" || apiErr.Type != "rate_limit_error" || apiErr.Param != "model" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if !apiErr.Temporary() || apiErr.RetryDelay().Seconds() != 7 {
		t.Errorf("temporary=%v retry=%v", apiErr.Temporary(), apiErr.RetryDelay())
	}
	if err.Error() == "" {
		t.Error("empty error string")
	}
}
