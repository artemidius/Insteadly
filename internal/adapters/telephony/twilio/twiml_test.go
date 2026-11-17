package twilio

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestVoiceTwiML(t *testing.T) {
	a := newTestAdapter(t, "")
	id := domain.CallID("0123456789abcdef0123456789abcdef")

	body, err := a.voiceTwiML(id)
	if err != nil {
		t.Fatalf("voiceTwiML: %v", err)
	}
	text := string(body)
	wantStream := "wss://public.example.test/webhooks/telephony/twilio/stream/" + string(id)
	if !strings.Contains(text, `url="`+wantStream+`"`) {
		t.Errorf("missing stream URL %q in %s", wantStream, text)
	}
	if !strings.Contains(text, `<Parameter name="call_id" value="`+string(id)+`">`) {
		t.Errorf("missing call_id parameter in %s", text)
	}
	var doc struct {
		Connect struct {
			Stream struct {
				URL        string `xml:"url,attr"`
				Parameters []struct {
					Name  string `xml:"name,attr"`
					Value string `xml:"value,attr"`
				} `xml:"Parameter"`
			} `xml:"Stream"`
		} `xml:"Connect"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal TwiML: %v", err)
	}
	if doc.Connect.Stream.URL != wantStream {
		t.Errorf("parsed URL = %q", doc.Connect.Stream.URL)
	}
	if strings.Contains(doc.Connect.Stream.URL, "?") {
		t.Errorf("stream URL must not carry a query string: %q", doc.Connect.Stream.URL)
	}
}

func TestDTMFTwiML(t *testing.T) {
	a := newTestAdapter(t, "")
	id := domain.CallID("0123456789abcdef0123456789abcdef")
	body, err := a.dtmfTwiML(id, "1w2#")
	if err != nil {
		t.Fatalf("dtmfTwiML: %v", err)
	}
	text := string(body)
	play := strings.Index(text, `<Play digits="1w2#">`)
	connect := strings.Index(text, "<Connect>")
	if play < 0 || connect < 0 || play > connect {
		t.Fatalf("expected Play before Connect, got %s", text)
	}
}

func TestHangupTwiML(t *testing.T) {
	a := newTestAdapter(t, "")
	body, err := a.hangupTwiML()
	if err != nil {
		t.Fatalf("hangupTwiML: %v", err)
	}
	if !strings.Contains(string(body), "<Hangup>") {
		t.Fatalf("expected Hangup TwiML, got %s", body)
	}
}

func TestStreamURLUsesWSS(t *testing.T) {
	a := newTestAdapter(t, "")
	id := domain.CallID("abc")
	if got := a.streamURL(id); got != "wss://public.example.test/webhooks/telephony/twilio/stream/abc" {
		t.Errorf("streamURL = %q", got)
	}
	if got := a.voiceURL(id); got != "https://public.example.test/webhooks/telephony/twilio/voice/abc" {
		t.Errorf("voiceURL = %q", got)
	}
}
