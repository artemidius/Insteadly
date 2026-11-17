package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/adapters/stub"
	"github.com/artemidius/insteadly/internal/adapters/telephony/twilio"
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

func buildTelephony(c config.Telephony, publicURL string, log *slog.Logger) (ports.Telephony, http.Handler, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTelephony(log), nil, nil
	case "twilio":
		if c.Twilio.Mode != "voice-api" {
			return nil, nil, fmt.Errorf("twilio mode %q: %w (lands in chunk C7)", c.Twilio.Mode, ports.ErrNotImplemented)
		}
		adapter, err := twilio.New(twilio.Config{
			AccountSID:    c.Twilio.AccountSID,
			AuthToken:     c.Twilio.AuthToken,
			FromNumber:    c.Twilio.PhoneNumber,
			PublicBaseURL: publicURL,
			RingTimeout:   time.Duration(c.Twilio.RingTimeoutSeconds) * time.Second,
			Logger:        log,
		})
		if err != nil {
			return nil, nil, err
		}
		return adapter, adapter.Handler(), nil
	default:
		return nil, nil, fmt.Errorf("unknown provider %q", c.Provider)
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
