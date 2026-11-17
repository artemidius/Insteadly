package runner

import (
	"github.com/artemidius/insteadly/internal/core/domain"
)

// Fixed, non-blank, non-secret fallback details. Provider bodies, model detail,
// transcripts, and patient data are never copied here or into logs.
const (
	detailAborted       = "the call was aborted"
	detailBusy          = "the clinic line was busy"
	detailNoAnswer      = "the clinic did not answer"
	detailRejected      = "the clinic rejected the call"
	detailIVRDeadEnd    = "the automated phone menu reached a dead end"
	detailCallError     = "the call failed"
	detailCompleted     = "the call ended before a dialogue outcome"
	detailUnknownReason = "the call ended without a usable reason"
	detailLimit         = "the conversation reached a limit"
)

func abortedOutcome() domain.Outcome {
	return domain.Outcome{Kind: domain.OutcomeAborted, Detail: detailAborted}
}

func failedOutcome(reason, detail string) domain.Outcome {
	if detail == "" {
		detail = detailCallError
	}
	return domain.Outcome{Kind: domain.OutcomeFailed, Detail: detail, FailReason: reason}
}

// fallbackOutcome derives exactly one outcome from a terminal call event. A provider
// error always wins as failed/error; EndReason values map to fixed details. Returned
// operational errors are only for true provider failures, not expected remote outcomes.
func fallbackOutcome(ev domain.CallEvent, have bool, providerErr bool) (domain.Outcome, error) {
	if providerErr {
		return failedOutcome("error", detailCallError), errProviderFailure
	}
	if !have {
		return failedOutcome("error", detailCallError), errMissingEnded
	}
	switch ev.EndReason {
	case domain.EndAborted:
		return abortedOutcome(), nil
	case domain.EndBusy:
		return failedOutcome("busy", detailBusy), nil
	case domain.EndNoAnswer:
		return failedOutcome("no_answer", detailNoAnswer), nil
	case domain.EndRejected:
		return failedOutcome("rejected", detailRejected), nil
	case domain.EndIVRDeadEnd:
		return failedOutcome("ivr_dead_end", detailIVRDeadEnd), nil
	case domain.EndError:
		return failedOutcome("error", detailCallError), errProviderFailure
	case domain.EndCompleted:
		return failedOutcome("other", detailCompleted), nil
	default:
		return failedOutcome("other", detailUnknownReason), nil
	}
}
