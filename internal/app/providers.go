package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	dialoguemistral "github.com/artemidius/insteadly/internal/adapters/dialogue/mistral"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	sttmistral "github.com/artemidius/insteadly/internal/adapters/stt/mistral"
	"github.com/artemidius/insteadly/internal/adapters/stub"
	"github.com/artemidius/insteadly/internal/adapters/telephony/twilio"
	ttsmistral "github.com/artemidius/insteadly/internal/adapters/tts/mistral"
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

func buildSTT(c config.STT, mistral config.Mistral, log *slog.Logger) (ports.SpeechToText, error) {
	switch c.Provider {
	case "stub":
		return stub.NewSTT(), nil
	case "mistral":
		adapter, err := sttmistral.New(sttmistral.Config{
			APIKey:           mistral.APIKey,
			BaseURL:          mistral.BaseURL,
			RealtimeModel:    mistral.STT.RealtimeModel,
			BatchModel:       mistral.STT.BatchModel,
			StreamingDelayMS: mistral.STT.StreamingDelayMS,
			Logger:           log,
		})
		if err != nil {
			return nil, err
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildTTS(c config.TTS, mistral config.Mistral, log *slog.Logger) (ports.TextToSpeech, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTTS(), nil
	case "mistral":
		adapter, err := ttsmistral.New(ttsmistral.Config{
			APIKey:       mistral.APIKey,
			BaseURL:      mistral.BaseURL,
			Model:        mistral.TTS.Model,
			DefaultVoice: mistral.TTS.DefaultVoice,
			Mode:         mistral.TTS.Mode,
			Logger:       log,
		})
		if err != nil {
			return nil, err
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func buildDialogue(c config.Dialogue, mistral config.Mistral, log *slog.Logger) (ports.Dialogue, error) {
	switch c.Provider {
	case "stub":
		return stub.NewDialogue(), nil
	case "mistral":
		adapter, err := dialoguemistral.New(dialoguemistral.Config{
			APIKey:  mistral.APIKey,
			BaseURL: mistral.BaseURL,
			Model:   mistral.Dialogue.Model,
			Logger:  log,
		})
		if err != nil {
			return nil, err
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}
