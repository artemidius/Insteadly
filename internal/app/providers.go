package app

import (
	"fmt"

	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/adapters/stub"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Each builder is one switch: adding a provider means one new case and one adapter
// package, never a core edit (DESIGN.md §2.3).

func buildStorage(c config.Storage) (ports.Storage, error) {
	switch c.Provider {
	case "local":
		return local.New(c.Local.Dir)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildTelephony(c config.Telephony) (ports.Telephony, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTelephony(nil), nil
	case "twilio":
		return nil, fmt.Errorf("provider %q: %w (lands in chunk C1)", c.Provider, ports.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildSTT(c config.STT) (ports.SpeechToText, error) {
	switch c.Provider {
	case "stub":
		return stub.NewSTT(), nil
	case "mistral":
		return nil, fmt.Errorf("provider %q: %w (lands in chunk C2)", c.Provider, ports.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildTTS(c config.TTS) (ports.TextToSpeech, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTTS(), nil
	case "mistral":
		return nil, fmt.Errorf("provider %q: %w (lands in chunk C2)", c.Provider, ports.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildDialogue(c config.Dialogue) (ports.Dialogue, error) {
	switch c.Provider {
	case "stub":
		return stub.NewDialogue(), nil
	case "mistral":
		return nil, fmt.Errorf("provider %q: %w (lands in chunk C2)", c.Provider, ports.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}
