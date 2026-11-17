package twilio

import (
	"encoding/xml"
	"net/url"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Webhook paths. They are adapter-owned transport details: the core never sees them.
const (
	webhookPrefix     = "/webhooks/telephony/twilio"
	voicePath         = webhookPrefix + "/voice/"
	statusPath        = webhookPrefix + "/status/"
	streamPath        = webhookPrefix + "/stream/"
	streamStatusPath  = webhookPrefix + "/stream-status/"
	callIDCustomParam = "call_id"
)

type twiMLResponse struct {
	XMLName xml.Name      `xml:"Response"`
	Play    *twiMLPlay    `xml:"Play,omitempty"`
	Connect *twiMLConnect `xml:"Connect,omitempty"`
	Hangup  *twiMLHangup  `xml:"Hangup,omitempty"`
}

type twiMLPlay struct {
	Digits string `xml:"digits,attr"`
}

type twiMLConnect struct {
	Stream twiMLStream `xml:"Stream"`
}

type twiMLStream struct {
	URL            string           `xml:"url,attr"`
	StatusCallback string           `xml:"statusCallback,attr,omitempty"`
	Parameters     []twiMLParameter `xml:"Parameter"`
}

type twiMLParameter struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type twiMLHangup struct{}

// voiceTwiML connects the answered call to a bidirectional Media Stream.
func (a *Adapter) voiceTwiML(id domain.CallID) ([]byte, error) {
	return marshalTwiML(twiMLResponse{
		Connect: &twiMLConnect{Stream: a.streamElement(id)},
	})
}

// dtmfTwiML plays DTMF digits and reconnects the Media Stream. Twilio's stream socket
// has no outbound DTMF message, so this call update is the supported mid-call path.
func (a *Adapter) dtmfTwiML(id domain.CallID, digits string) ([]byte, error) {
	return marshalTwiML(twiMLResponse{
		Play:    &twiMLPlay{Digits: digits},
		Connect: &twiMLConnect{Stream: a.streamElement(id)},
	})
}

func (a *Adapter) hangupTwiML() ([]byte, error) {
	return marshalTwiML(twiMLResponse{Hangup: &twiMLHangup{}})
}

func (a *Adapter) streamElement(id domain.CallID) twiMLStream {
	return twiMLStream{
		URL:            a.streamURL(id),
		StatusCallback: a.absoluteURL(streamStatusPath + string(id)),
		Parameters: []twiMLParameter{
			{Name: callIDCustomParam, Value: string(id)},
		},
	}
}

func marshalTwiML(v twiMLResponse) ([]byte, error) {
	data, err := xml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), data...), nil
}

func (a *Adapter) absoluteURL(path string) string {
	return a.publicBaseURL + path
}

func (a *Adapter) voiceURL(id domain.CallID) string {
	return a.absoluteURL(voicePath + string(id))
}

func (a *Adapter) statusURL(id domain.CallID) string {
	return a.absoluteURL(statusPath + string(id))
}

// streamURL derives the WSS endpoint from the public https origin. Twilio accepts only
// wss in the TwiML <Stream> url and does not support query strings there.
func (a *Adapter) streamURL(id domain.CallID) string {
	u, err := url.Parse(a.publicBaseURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = streamPath + string(id)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
