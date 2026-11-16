package domain

// Tool is a tool the Dialogue model may invoke (propose_slot, confirm_booking,
// capture_insurance, escalate_to_human, ...). The exact tool set is resolved in C3;
// the type is needed now so the Dialogue port can be declared.
type Tool struct {
	Name        string
	Description string
	Parameters  []byte // JSON Schema for the arguments
}
