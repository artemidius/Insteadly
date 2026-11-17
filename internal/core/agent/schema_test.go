package agent

import (
	"encoding/json"
	"testing"
)

func TestToolDefinitionsExactSurface(t *testing.T) {
	tools := ToolDefinitions()
	if len(tools) != 4 {
		t.Fatalf("len(tools) = %d, want 4", len(tools))
	}
	wantNames := []string{ToolProposeSlot, ToolConfirmBooking, ToolCaptureInfo, ToolEscalateToHuman}
	wantSchemas := []string{
		`{"type":"object","additionalProperties":false,"properties":{"start":{"type":"string","format":"date-time"},"end":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["start","end"]}`,
		`{"type":"object","additionalProperties":false,"properties":{"start":{"type":"string","format":"date-time"},"end":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"},"confirmation_code":{"type":"string"}},"required":["start","end"]}`,
		`{"type":"object","additionalProperties":false,"properties":{"field":{"type":"string","enum":["full_name","phone","birth_date","insurance_number","reason","note"]},"value":{"type":"string","minLength":1,"maxLength":2048}},"required":["field","value"]}`,
		`{"type":"object","additionalProperties":false,"properties":{"reason":{"type":"string","minLength":1},"urgent":{"type":"boolean"}},"required":["reason"]}`,
	}
	for i, tool := range tools {
		if tool.Name != wantNames[i] {
			t.Errorf("tool[%d].Name = %q, want %q", i, tool.Name, wantNames[i])
		}
		if tool.Name == ToolFinishCall {
			t.Error("finish_call must not be an ordinary tool")
		}
		if string(tool.Parameters) != wantSchemas[i] {
			t.Errorf("tool[%d] schema mismatch:\n got %s\nwant %s", i, tool.Parameters, wantSchemas[i])
		}
		var decoded map[string]any
		if err := json.Unmarshal(tool.Parameters, &decoded); err != nil {
			t.Errorf("tool[%d] schema is not JSON: %v", i, err)
		}
		if tool.Description == "" {
			t.Errorf("tool[%d] has no description", i)
		}
	}
}

func TestToolDefinitionsFreshSlices(t *testing.T) {
	first := ToolDefinitions()
	first[0].Name = "mutated"
	first[0].Parameters[0] = 'x'
	second := ToolDefinitions()
	if second[0].Name != ToolProposeSlot {
		t.Errorf("name mutated across calls: %q", second[0].Name)
	}
	if string(second[0].Parameters) != string(proposeSlotSchema) {
		t.Error("schema bytes mutated across calls")
	}
}

func TestOutcomeSchemaExact(t *testing.T) {
	got := OutcomeSchema()
	want := `{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["booked","callback_needed","failed","aborted"]},"detail":{"type":"string","minLength":1},"booked_slot":{"type":"object","additionalProperties":false,"properties":{"date_time":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["date_time"]},"callback_at":{"type":"string","format":"date-time"},"fail_reason":{"type":"string","minLength":1}},"required":["kind","detail"]}`
	if string(got) != want {
		t.Fatalf("outcome schema mismatch:\n got %s\nwant %s", got, want)
	}
	got[0] = 'x'
	if string(OutcomeSchema()) != want {
		t.Error("outcome schema bytes mutated across calls")
	}
}
