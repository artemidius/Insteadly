package agent

// Tool names owned by the core. finish_call is reserved by the dialogue adapter and is
// never part of the ordinary tool definitions or the executor.
const (
	ToolProposeSlot     = "propose_slot"
	ToolConfirmBooking  = "confirm_booking"
	ToolCaptureInfo     = "capture_info"
	ToolEscalateToHuman = "escalate_to_human"
	ToolFinishCall      = "finish_call"
)

// outcomeSchema is the canonical C2 §3.8 outcome schema. The dialogue adapter receives
// these bytes verbatim as the parameters of its reserved finish_call tool.
var outcomeSchema = []byte(`{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["booked","callback_needed","failed","aborted"]},"detail":{"type":"string","minLength":1},"booked_slot":{"type":"object","additionalProperties":false,"properties":{"date_time":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["date_time"]},"callback_at":{"type":"string","format":"date-time"},"fail_reason":{"type":"string","minLength":1}},"required":["kind","detail"]}`)

// OutcomeSchema returns a fresh copy so a dialogue adapter cannot mutate core state.
func OutcomeSchema() []byte { return append([]byte(nil), outcomeSchema...) }
