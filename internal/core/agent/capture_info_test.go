package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestCaptureInfoPersistsAllowedFields(t *testing.T) {
	order := testOrder()
	now := fixedNow
	exec, store := newTestExecutor(t, order, &now)

	cases := map[string]string{
		"full_name":        "Erika Musterfrau",
		"phone":            "+43660111222333",
		"birth_date":       "1975-05-05",
		"insurance_number": "AT-999",
		"reason":           "Kontrolle",
	}
	for field, value := range cases {
		args := `{"field":"` + field + `","value":"` + value + `"}`
		got := string(execute(t, exec, ToolCaptureInfo, args))
		if got != `{"ok":true,"field":"`+field+`"}` {
			t.Errorf("%s result = %s", field, got)
		}
		stored, err := store.Get(context.Background(), order.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Status != order.Status || stored.Attempts != order.Attempts {
			t.Errorf("%s changed status/attempts: %+v", field, stored)
		}
		switch field {
		case "full_name":
			if stored.Patient.FullName != value {
				t.Errorf("full_name = %q", stored.Patient.FullName)
			}
		case "phone":
			if stored.Patient.Phone != value {
				t.Errorf("phone = %q", stored.Patient.Phone)
			}
		case "birth_date":
			if stored.Patient.BirthDate != value {
				t.Errorf("birth_date = %q", stored.Patient.BirthDate)
			}
		case "insurance_number":
			if stored.Patient.InsuranceNumber != value {
				t.Errorf("insurance_number = %q", stored.Patient.InsuranceNumber)
			}
		case "reason":
			if stored.Patient.Reason != value {
				t.Errorf("reason = %q", stored.Patient.Reason)
			}
		}
	}
}

func TestCaptureInfoTrimsAndKeepsValueOutOfResult(t *testing.T) {
	now := fixedNow
	exec, store := newTestExecutor(t, testOrder(), &now)
	got := string(execute(t, exec, ToolCaptureInfo, `{"field":"reason","value":"  Kontrolle  "}`))
	if got != `{"ok":true,"field":"reason"}` {
		t.Fatalf("result = %s", got)
	}
	stored, _ := store.Get(context.Background(), exec.orderID)
	if stored.Patient.Reason != "Kontrolle" {
		t.Errorf("trimmed reason = %q", stored.Patient.Reason)
	}
	if strings.Contains(got, "Kontrolle") {
		t.Error("result leaked the captured value")
	}
}

func TestCaptureInfoNoteIsPerCallOnly(t *testing.T) {
	now := fixedNow
	exec, store := newTestExecutor(t, testOrder(), &now)
	before := store.updateCount()
	got := string(execute(t, exec, ToolCaptureInfo, `{"field":"note","value":"bringt Befund mit"}`))
	if got != `{"ok":true,"field":"note"}` {
		t.Fatalf("result = %s", got)
	}
	if store.updateCount() != before {
		t.Error("note must not persist to the order store")
	}
	if len(exec.notes) != 1 || exec.notes[0] != "bringt Befund mit" {
		t.Errorf("notes = %v", exec.notes)
	}
}

func TestCaptureInfoInvalidValues(t *testing.T) {
	now := fixedNow
	exec, store := newTestExecutor(t, testOrder(), &now)
	for name, args := range map[string]string{
		"unknown field":   `{"field":"address","value":"x"}`,
		"blank value":     `{"field":"reason","value":"   "}`,
		"wrong type":      `{"field":"reason","value":5}`,
		"missing value":   `{"field":"reason"}`,
		"missing field":   `{"value":"x"}`,
		"unknown arg":     `{"field":"reason","value":"x","extra":1}`,
		"trailing":        `{"field":"reason","value":"x"} {}`,
		"bad birth date":  `{"field":"birth_date","value":"05.05.1975"}`,
		"impossible date": `{"field":"birth_date","value":"2027-13-45"}`,
	} {
		t.Run(name, func(t *testing.T) {
			want := `{"ok":false,"error":"invalid_value"}`
			if name == "wrong type" || name == "missing value" || name == "missing field" ||
				name == "unknown arg" || name == "trailing" {
				want = `{"ok":false,"error":"invalid_arguments"}`
			}
			if got := string(execute(t, exec, ToolCaptureInfo, args)); got != want {
				t.Errorf("result = %s, want %s", got, want)
			}
		})
	}
	if store.updateCount() != 0 {
		t.Errorf("invalid captures wrote %d updates", store.updateCount())
	}
}

func TestCaptureInfoLengthBoundary(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	exact := strings.Repeat("ä", maxCaptureValueRunes)
	args := `{"field":"reason","value":"` + exact + `"}`
	if got := string(execute(t, exec, ToolCaptureInfo, args)); !strings.Contains(got, `"ok":true`) {
		t.Errorf("2048 runes rejected: %s", got)
	}
	over := strings.Repeat("ä", maxCaptureValueRunes+1)
	args = `{"field":"reason","value":"` + over + `"}`
	if got := string(execute(t, exec, ToolCaptureInfo, args)); got != `{"ok":false,"error":"invalid_value"}` {
		t.Errorf("2049 runes accepted: %s", got)
	}
}

func TestCaptureInfoStorageFailureAborts(t *testing.T) {
	now := fixedNow
	exec, store := newTestExecutor(t, testOrder(), &now)
	sentinel := errors.New("disk full")
	store.updateFn = func(*domain.Order) error { return sentinel }
	_, err := exec.Execute(context.Background(), ports.ToolCall{
		Name: ToolCaptureInfo, Arguments: []byte(`{"field":"reason","value":"Kontrolle"}`),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want storage failure", err)
	}
}

func TestEscalateToHumanAlwaysUnavailable(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	got := string(execute(t, exec, ToolEscalateToHuman, `{"reason":"komplizierter Fall","urgent":true}`))
	if got != `{"ok":false,"error":"unavailable"}` {
		t.Errorf("result = %s", got)
	}
	if !exec.escalated || !exec.urgent {
		t.Errorf("escalation state = escalated:%v urgent:%v", exec.escalated, exec.urgent)
	}

	for _, args := range []string{`{}`, `{"reason":"   "}`, `{"reason":5}`, `{"reason":"x","extra":1}`} {
		if got := string(execute(t, exec, ToolEscalateToHuman, args)); got != `{"ok":false,"error":"invalid_arguments"}` {
			t.Errorf("args %s -> %s", args, got)
		}
	}
	// urgent is optional and defaults to false.
	exec2, _ := newTestExecutor(t, testOrder(), &now)
	execute(t, exec2, ToolEscalateToHuman, `{"reason":"x"}`)
	if exec2.urgent {
		t.Error("urgent should default false")
	}
}
