//go:build live

package app_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/httpapi"
)

// TestLiveCall places one real, short call through Twilio. It is intentionally excluded
// from normal builds/tests:
//
//	go test -tags live -count=1 -run TestLiveCall -v ./internal/app
//
// Required: LIVE_TWILIO=1 (unprefixed so the strict config loader ignores it), INSTEADLY_TELEPHONY_PROVIDER=twilio,
// INSTEADLY_HTTP_PUBLIC_URL=<public tunnel>, TWILIO_* credentials exported as
// INSTEADLY_TWILIO_*, and TEST_PHONE set. Optional: LIVE_DTMF=1.
//
// The test starts the real HTTP server on cfg.HTTP.Addr (default 127.0.0.1:8080) because
// the tunnel must point at that address. It costs trial credit and rings the owner's phone.
func TestLiveCall(t *testing.T) {
	if os.Getenv("LIVE_TWILIO") != "1" {
		t.Skip("set LIVE_TWILIO=1 to place a real Twilio test call")
	}
	to := os.Getenv("TEST_PHONE")
	if to == "" {
		t.Skip("TEST_PHONE is not set")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Telephony.Provider != "twilio" {
		t.Fatal("INSTEADLY_TELEPHONY_PROVIDER must be twilio for the live test")
	}
	if cfg.HTTP.PublicURL == "" {
		t.Fatal("INSTEADLY_HTTP_PUBLIC_URL is required")
	}

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		t.Fatalf("listen %s: %v", cfg.HTTP.Addr, err)
	}
	srv := httpapi.New(cfg, a, "live-test")
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	t.Logf("serving webhooks at %s (local %s)", cfg.HTTP.PublicURL, cfg.HTTP.Addr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := a.Telephony.Dial(ctx, domain.Order{
		ID:     domain.NewOrderID(),
		Clinic: domain.Clinic{Name: "Insteadly live test", Phone: to},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Hangup(context.Background()) })

	events := make(chan domain.CallEvent, 64)
	go func() {
		for ev := range sess.Events() {
			events <- ev
		}
	}()
	go func() {
		for range sess.Inbound() {
		}
	}()

	var streamReady <-chan struct{}
	if withStream, ok := sess.(interface{ StreamReady() <-chan struct{} }); ok {
		streamReady = withStream.StreamReady()
	}

	answered := false
	toneResult := make(chan error, 1)
	deadline := time.After(90 * time.Second)

	for {
		select {
		case ev := <-events:
			t.Logf("event kind=%s dtmf=%q end=%q", ev.Kind, ev.DTMF, ev.EndReason)
			switch ev.Kind {
			case domain.EventAnswered:
				if answered {
					continue
				}
				answered = true
				go func() {
					if streamReady != nil {
						select {
						case <-streamReady:
							log.Print("live: media stream attached")
						case <-time.After(20 * time.Second):
							toneResult <- fmt.Errorf("media stream did not attach within 20s")
							return
						}
					}
					time.Sleep(500 * time.Millisecond)
					toneResult <- sendTone(sess)
				}()
			case domain.EventEnded:
				if !answered {
					t.Fatalf("call ended before answer: %q", ev.EndReason)
				}
				if streamReady != nil {
					select {
					case <-streamReady:
					default:
						t.Fatal("Media Stream never attached; a Twilio trial account strips <Stream> from TwiML, so live audio requires an upgraded account")
					}
				}
				select {
				case err := <-toneResult:
					if err != nil {
						t.Fatalf("test tone failed: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("test tone did not finish before the call ended")
				}
				t.Logf("live call completed with reason %q", ev.EndReason)
				return
			}
		case <-deadline:
			_ = sess.Hangup(context.Background())
			t.Fatal("live call did not finish within 90s")
		}
	}
}

func sendTone(sess interface {
	SendAudio(context.Context, domain.AudioChunk) error
	SendDTMF(context.Context, string) error
	Hangup(context.Context) error
}) error {
	const (
		rate     = 16000
		freq     = 440.0
		duration = time.Second
	)
	frames := int(duration.Seconds() * 1000 / 20)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for f := 0; f < frames; f++ {
		buf := make([]byte, domain.FrameBytes)
		for i := 0; i < domain.FrameBytes/2; i++ {
			n := f*domain.FrameBytes/2 + i
			s := int16(12000 * math.Sin(2*math.Pi*freq*float64(n)/rate))
			binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
		}
		if err := sess.SendAudio(ctx, domain.AudioChunk{Data: buf}); err != nil {
			return fmt.Errorf("send tone: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	if os.Getenv("LIVE_DTMF") == "1" {
		if err := sess.SendDTMF(ctx, "1"); err != nil {
			return fmt.Errorf("send dtmf: %w", err)
		}
		log.Print("live: sent DTMF digit 1")
		time.Sleep(2 * time.Second)
	}
	return sess.Hangup(ctx)
}
