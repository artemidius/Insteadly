package mistral

import (
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func allowAll() map[string]bool {
	return map[string]bool{"capture_info": true, reservedToolName: true}
}

func sb(s string) *strings.Builder {
	var b strings.Builder
	b.WriteString(s)
	return &b
}

func TestFinalizeCallsSingleToolCall(t *testing.T) {
	calls := []assembledCall{{
		index: 0, id: "call_1", name: "capture_info", typ: "function",
		args: *sb(`{"field":"reason","value":"Kontrolle"}`),
	}}
	assistant, res, err := finalizeCalls(chatMessage{Role: "assistant"}, "", calls, allowAll())
	if err != nil {
		t.Fatalf("finalizeCalls: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.Outcome != nil {
		t.Fatalf("result = %+v", res)
	}
	if res.ToolCalls[0].Name != "capture_info" || string(res.ToolCalls[0].Arguments) != `{"field":"reason","value":"Kontrolle"}` {
		t.Errorf("tool call = %+v", res.ToolCalls[0])
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Arguments != `{"field":"reason","value":"Kontrolle"}` {
		t.Errorf("assistant tool calls = %+v", assistant.ToolCalls)
	}
}

func TestFinalizeCallsFinishCall(t *testing.T) {
	calls := []assembledCall{{
		index: 0, id: "call_1", name: reservedToolName, typ: "function",
		args: *sb(`{"kind":"booked","detail":"ok","booked_slot":{"date_time":"2026-11-20T09:00:00Z"}}`),
	}}
	_, res, err := finalizeCalls(chatMessage{Role: "assistant"}, "", calls, allowAll())
	if err != nil {
		t.Fatalf("finalizeCalls: %v", err)
	}
	if res.Outcome == nil || res.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", res.Outcome)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("finish_call must not be returned as a ToolCall: %+v", res.ToolCalls)
	}
}

func TestFinalizeCallsRejects(t *testing.T) {
	obj := `{"a":1}`
	cases := []struct {
		name  string
		calls []assembledCall
	}{
		{"duplicate id", []assembledCall{
			{index: 0, id: "x", name: "capture_info", args: *sb(obj)},
			{index: 1, id: "x", name: "capture_info", args: *sb(obj)},
		}},
		{"missing id", []assembledCall{
			{index: 0, name: "capture_info", args: *sb(obj)},
		}},
		{"missing name", []assembledCall{
			{index: 0, id: "x", args: *sb(obj)},
		}},
		{"non-contiguous", []assembledCall{
			{index: 1, id: "x", name: "capture_info", args: *sb(obj)},
		}},
		{"unknown name", []assembledCall{
			{index: 0, id: "x", name: "hack", args: *sb(obj)},
		}},
		{"non-object args", []assembledCall{
			{index: 0, id: "x", name: "capture_info", args: *sb(`[1,2]`)},
		}},
		{"bad type", []assembledCall{
			{index: 0, id: "x", name: "capture_info", typ: "image", args: *sb(obj)},
		}},
		{"finish_call combined", []assembledCall{
			{index: 0, id: "a", name: "capture_info", args: *sb(obj)},
			{index: 1, id: "b", name: reservedToolName, args: *sb(`{"kind":"aborted","detail":"d"}`)},
		}},
		{"finish_call twice", []assembledCall{
			{index: 0, id: "a", name: reservedToolName, args: *sb(`{"kind":"aborted","detail":"d"}`)},
			{index: 1, id: "b", name: reservedToolName, args: *sb(`{"kind":"aborted","detail":"d"}`)},
		}},
		{"finish_call bad outcome", []assembledCall{
			{index: 0, id: "a", name: reservedToolName, args: *sb(`{"kind":"aborted"}`)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := finalizeCalls(chatMessage{}, "", tc.calls, allowAll()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestIsJSONObject(t *testing.T) {
	if !isJSONObject(`{"a":1}`) {
		t.Error(`{"a":1} must be a JSON object`)
	}
	for _, bad := range []string{`[1]`, `"s"`, `1`, ``, `{"a":1} {}`} {
		if isJSONObject(bad) {
			t.Errorf("accepted %q as JSON object", bad)
		}
	}
}
