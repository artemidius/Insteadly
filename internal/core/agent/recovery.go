package agent

import (
	"context"
	"fmt"

	"github.com/artemidius/insteadly/internal/core/ports"
)

// recoveryReason is the stable internal code for one bounded outcome repair. Only these
// model/conversation failures are recoverable; transport, STT/TTS, clear/mark, caller
// cancellation, and terminal call events are not.
type recoveryReason int

const (
	recoveryInvalidOutcome recoveryReason = iota
	recoveryToolConsistency
	recoveryEmptyTurn
	recoveryStall
)

func (r recoveryReason) code() string {
	switch r {
	case recoveryInvalidOutcome:
		return "invalid_outcome"
	case recoveryToolConsistency:
		return "tool_consistency"
	case recoveryEmptyTurn:
		return "empty_turn"
	case recoveryStall:
		return "model_stall"
	default:
		return "unknown"
	}
}

// recoveryInstruction is the fixed, core-owned German control instruction sent as a
// system-role message. It never contains provider bodies, model prose, validation strings,
// transcripts, patient values, or credentials.
func recoveryInstruction(r recoveryReason) string {
	switch r {
	case recoveryInvalidOutcome:
		return "Dein letzter Abschluss war ungültig. Rufe finish_call erneut mit genau einem vollständigen, schema-konformen Ergebnis auf und beachte alle bedingten Felder."
	case recoveryToolConsistency:
		return "Dein letztes Ergebnis passt nicht zu den tatsächlich ausgeführten Werkzeugen. Stelle das Ergebnis konsistent zum tatsächlichen Vorschlag und zur tatsächlichen Bestätigung her; erfinde keine Buchung und ändere keine bereits erfassten Angaben."
	case recoveryEmptyTurn:
		return "Deine letzte Antwort war leer. Antworte jetzt kurz und hilfreich oder schließe mit genau einem gültigen finish_call ab."
	case recoveryStall:
		return "Deine letzte Antwort ist ausgeblieben. Setze das Gespräch ohne Wiederholung bereits ausgeführter Werkzeuge fort und antworte oder schließe mit genau einem gültigen finish_call ab."
	default:
		return "Setze das Gespräch mit genau einem gültigen finish_call fort."
	}
}

// recoveryBudget is the per-call, not per-user-turn, outcome-recovery counter. The initial
// bad turn does not count; a third consecutive need exhausts the budget.
type recoveryBudget struct {
	used int
}

// next consumes one recovery round. It returns errOutcomeRecoveryExhausted (wrapped as an
// agent.LimitError by the caller) when the configured maximum has already been used.
func (b *recoveryBudget) next(max int) error {
	if b.used >= max {
		return errOutcomeRecoveryExhausted
	}
	b.used++
	return nil
}

// recoverOutcome asks the dialogue adapter for one control turn after a recoverable model
// failure. The adapter preserves the triggering user/tool candidate and never represents
// the instruction as user speech.
func (a *Agent) recoverOutcome(ctx context.Context, session ports.DialogueSession, reason recoveryReason) (ports.AssistantTurn, error) {
	turn, err := session.SendControl(ctx, recoveryInstruction(reason))
	if err != nil {
		return nil, fmt.Errorf("dialogue control turn: %w", err)
	}
	return turn, nil
}
