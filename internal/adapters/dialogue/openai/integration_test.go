package openai

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestIntegrationFullToolThenFinishCall(t *testing.T) {
	phase := 0
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch phase {
		case 0:
			writeSSE(w, toolCallSSE("capture_info", "call_1", `{"field":"reason","value":"Kontrolle"}`, "tool_calls"))
		case 1:
			writeSSE(w, toolCallSSE("finish_call", "call_f", `{"kind":"booked","detail":"ok","booked_slot":{"date_time":"2026-11-20T09:00:00Z"}}`, "tool_calls"))
		default:
			t.Error("unexpected third request")
		}
		phase++
	})

	s := newSession(t, a, []domain.Tool{captureInfoTool()}, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "Kontrolle")
	res, _, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "capture_info" {
		t.Fatalf("turn 1 calls = %+v", res.ToolCalls)
	}

	turn2, _ := s.SubmitToolResults(context.Background(), []ports.ToolResult{{ID: "call_1", Result: []byte(`{"ok":true}`)}})
	res2, _, err := drainTurn(turn2)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Outcome == nil || res2.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", res2.Outcome)
	}
}

func TestDialogueHTTPErrors(t *testing.T) {
	cases := []struct {
		status int
		temp   bool
	}{
		{401, false},
		{429, true},
		{500, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"err"}`))
			})
			s := newSession(t, a, nil, outcomeSchema(), "")
			turn, _ := s.SendUserTurn(context.Background(), "hi")
			_, _, err := drainTurn(turn)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %T %v, want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Temporary() != tc.temp {
				t.Errorf("apiErr = %+v", apiErr)
			}
		})
	}
}

func TestDialogueContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(ctx, "hi")
	cancel()
	_, _, err := drainTurn(turn)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestDialogueTruncatedTurn(t *testing.T) {
	cases := map[string]string{
		"premature eof":       "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
		"length finish":       textSSE([]string{"hi"}, "length"),
		"model_length finish": textSSE([]string{"hi"}, "model_length"),
		"no done before eof":  textSSE([]string{"hi"}, "stop")[:len(textSSE([]string{"hi"}, "stop"))-len("data: [DONE]\n\n")],
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, body)
			})
			s := newSession(t, a, nil, outcomeSchema(), "")
			turn, _ := s.SendUserTurn(context.Background(), "hi")
			_, _, err := drainTurn(turn)
			if err == nil {
				t.Fatal("expected truncation error")
			}
			if name == "length finish" || name == "model_length finish" {
				if !errors.Is(err, ErrTurnTruncated) {
					t.Errorf("err = %v, want ErrTurnTruncated", err)
				}
			}
		})
	}
}

func TestDialogueMalformedSSE(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, "data: not-json\n\n")
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "hi")
	_, _, err := drainTurn(turn)
	if err == nil {
		t.Fatal("malformed SSE must error")
	}
}

var _ ports.Dialogue = (*Adapter)(nil)
var _ ports.DialogueSession = (*session)(nil)
var _ ports.AssistantTurn = (*turn)(nil)
