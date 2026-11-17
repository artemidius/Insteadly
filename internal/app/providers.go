package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	dialoguemistral "github.com/artemidius/insteadly/internal/adapters/dialogue/mistral"
	dialogueopenai "github.com/artemidius/insteadly/internal/adapters/dialogue/openai"
	notifylog "github.com/artemidius/insteadly/internal/adapters/notify/log"
	notifywebhook "github.com/artemidius/insteadly/internal/adapters/notify/webhook"
	"github.com/artemidius/insteadly/internal/adapters/password/bcrypt"
	paymentsstripe "github.com/artemidius/insteadly/internal/adapters/payments/stripe"
	paymentsstub "github.com/artemidius/insteadly/internal/adapters/payments/stub"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/adapters/storage/postgres"
	sttmistral "github.com/artemidius/insteadly/internal/adapters/stt/mistral"
	"github.com/artemidius/insteadly/internal/adapters/stub"
	"github.com/artemidius/insteadly/internal/adapters/telephony/telnyx"
	"github.com/artemidius/insteadly/internal/adapters/telephony/twilio"
	ttsmistral "github.com/artemidius/insteadly/internal/adapters/tts/mistral"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/recorder"
)

// Each builder is one switch: adding a provider means one new case and one adapter
// package, never a core edit (DESIGN.md §2.3).

// buildStorage returns the order/blob storage, the scheduler store, the auth store, and a
// closer for the durable handle. In postgres mode blobs stay on the local adapter (C5 does
// not build a Postgres blob store), so Local.Dir remains required.
func buildStorage(ctx context.Context, c config.Storage, log *slog.Logger) (ports.Storage, ports.SchedulerStore, ports.AuthStore, io.Closer, error) {
	switch c.Provider {
	case "local":
		store, err := local.New(c.Local.Dir)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return store, store, store, nopCloser{}, nil
	case "postgres":
		blobs, err := local.New(c.Local.Dir)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		openCtx, cancel := context.WithTimeout(ctx, storageOpenTimeout)
		defer cancel()
		store, err := postgres.New(openCtx, postgres.Config{DSN: c.Postgres.DSN, Logger: log})
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if err := store.Migrate(openCtx); err != nil {
			_ = store.Close()
			return nil, nil, nil, nil, err
		}
		return compositeStorage{orders: store, blobs: blobs}, store, store, store, nil
	default:
		return nil, nil, nil, nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

// compositeStorage is the postgres-mode ports.Storage: orders on Postgres, blobs local.
type compositeStorage struct {
	orders ports.OrderStore
	blobs  ports.BlobStore
}

func (s compositeStorage) Orders() ports.OrderStore { return s.orders }

func (s compositeStorage) Blobs() ports.BlobStore { return s.blobs }

// nopCloser is the local-storage closer: there is nothing to release.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func buildTelephony(c config.Telephony, publicURL string, log *slog.Logger) (ports.Telephony, http.Handler, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTelephony(log), nil, nil
	case "telnyx":
		adapter, err := telnyx.New(telnyx.Config{
			APIKey:           c.Telnyx.APIKey,
			ConnectionID:     c.Telnyx.ConnectionID,
			FromNumber:       c.Telnyx.FromNumber,
			PublicBaseURL:    publicURL,
			WebhookPublicKey: c.Telnyx.WebhookPublicKey,
			RingTimeout:      time.Duration(c.Telnyx.RingTimeoutSeconds) * time.Second,
			Logger:           log,
		})
		if err != nil {
			return nil, nil, err
		}
		return adapter, adapter.Handler(), nil
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

func buildDialogue(c config.Dialogue, mistral config.Mistral, openai config.OpenAI, log *slog.Logger) (ports.Dialogue, error) {
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
	case "openai":
		adapter, err := dialogueopenai.New(dialogueopenai.Config{
			APIKey:  openai.APIKey,
			BaseURL: openai.BaseURL,
			Model:   openai.DialogueModel,
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

// buildRecorder selects the recording capture seam. It is always wired: with every flag
// off (the default) recorder.Wrap returns the dialed session unchanged, and a per-order
// override can still enable capture. A nil recorder would make per-order overrides
// impossible.
func buildRecorder(policy config.Policy, stt ports.SpeechToText, blobs ports.BlobStore, log *slog.Logger) recorder.Recorder {
	return recorder.New(stt, blobs, recorder.Policy{
		Recording:  policy.RecordingEnabled,
		Transcript: policy.TranscriptEnabled,
		Audio:      policy.AudioEnabled,
	}, log)
}

// buildNotifier selects the notification adapter. "off" (the default) returns nil: no
// notifier, no worker, no behavior change.
func buildNotifier(c config.Notify, log *slog.Logger) (ports.Notifier, error) {
	switch c.Provider {
	case "off", "":
		return nil, nil
	case "log":
		return notifylog.New(log), nil
	case "webhook":
		return notifywebhook.New(c.Webhook.URL, c.Webhook.Channel, log)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}

// buildPasswordHasher selects the password hasher. bcrypt lives in the adapter layer; the
// core auth service only sees ports.PasswordHasher. A zero cost means the default (12).
func buildPasswordHasher(c config.Auth, log *slog.Logger) (ports.PasswordHasher, error) {
	hasher, err := bcrypt.New(c.BcryptCost)
	if err != nil {
		return nil, err
	}
	cost := c.BcryptCost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	if log != nil {
		log.Debug("auth: password hasher configured", "bcrypt_cost", cost)
	}
	return hasher, nil
}

// buildPayments selects the payments adapter. Stripe keys are consumed only here and in
// the adapter; config merely trimmed and non-empty-checked them.
func buildPayments(c config.Payments, publicURL string, log *slog.Logger) (ports.Payments, error) {
	switch c.Provider {
	case "stub", "":
		return paymentsstub.New(log), nil
	case "stripe":
		base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
		if base == "" {
			return nil, fmt.Errorf("stripe: http.public_url is required for checkout redirect URLs")
		}
		adapter, err := paymentsstripe.New(paymentsstripe.Config{
			SecretKey:     c.Stripe.SecretKey,
			WebhookSecret: c.Stripe.WebhookSecret,
			PriceID:       c.Stripe.PriceID,
			SuccessURL:    base + "/dashboard",
			CancelURL:     base + "/orders",
			Logger:        log,
		})
		if err != nil {
			return nil, err
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}
