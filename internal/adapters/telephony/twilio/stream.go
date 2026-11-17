package twilio

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const (
	maxWebSocketMessage = 64 << 10
	maxMediaPayload     = 8 << 10
	writeTimeout        = 5 * time.Second
)

type streamConn struct {
	session *session
	conn    *websocket.Conn

	mu        sync.Mutex
	streamSID string
	dec       frameDecoder

	// sendMu serializes every outbound wire message (media, mark, clear) so their order
	// on the stream is exactly the order the session writer submitted them.
	sendMu sync.Mutex
}

func newStreamConn(s *session, c *websocket.Conn) *streamConn {
	return &streamConn{session: s, conn: c}
}

func (c *streamConn) setStreamSID(sid string) {
	c.mu.Lock()
	c.streamSID = sid
	c.mu.Unlock()
}

func (c *streamConn) close() {
	_ = c.conn.CloseNow()
}

// writeMedia sends one µ-law frame to Twilio.
func (c *streamConn) writeMedia(payload []byte) error {
	var msg outboundMedia
	msg.Event = "media"
	msg.Media.Payload = base64.StdEncoding.EncodeToString(payload)
	return c.write(&msg)
}

// writeMark sends one outbound playback mark.
func (c *streamConn) writeMark(name string) error {
	var msg outboundMark
	msg.Event = "mark"
	msg.Mark.Name = name
	return c.write(&msg)
}

// writeClear sends the provider clear for this stream.
func (c *streamConn) writeClear() error {
	var msg outboundClear
	msg.Event = "clear"
	return c.write(&msg)
}

// write serializes one outbound JSON message and injects the current stream SID.
func (c *streamConn) write(v any) error {
	c.mu.Lock()
	sid := c.streamSID
	c.mu.Unlock()
	if sid == "" {
		return errors.New("stream has not started")
	}
	switch msg := v.(type) {
	case *outboundMedia:
		msg.StreamSID = sid
	case *outboundMark:
		msg.StreamSID = sid
	case *outboundClear:
		msg.StreamSID = sid
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, c.conn, v)
}

type outboundMedia struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
	Media     struct {
		Payload string `json:"payload"`
	} `json:"media"`
}

type outboundMark struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
	Mark      struct {
		Name string `json:"name"`
	} `json:"mark"`
}

type outboundClear struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
}

type inboundEnvelope struct {
	Event     string        `json:"event"`
	StreamSID string        `json:"streamSid"`
	Start     *startPayload `json:"start"`
	Media     *mediaPayload `json:"media"`
	DTMF      *dtmfPayload  `json:"dtmf"`
	Mark      *markPayload  `json:"mark"`
	Stop      *stopPayload  `json:"stop"`
}

type startPayload struct {
	StreamSID        string            `json:"streamSid"`
	AccountSID       string            `json:"accountSid"`
	CallSID          string            `json:"callSid"`
	Tracks           []string          `json:"tracks"`
	MediaFormat      mediaFormat       `json:"mediaFormat"`
	CustomParameters map[string]string `json:"customParameters"`
}

type mediaFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

type mediaPayload struct {
	Track     string `json:"track"`
	Chunk     string `json:"chunk"`
	Timestamp string `json:"timestamp"`
	Payload   string `json:"payload"`
}

type dtmfPayload struct {
	Track string `json:"track"`
	Digit string `json:"digit"`
}

type markPayload struct {
	Name string `json:"name"`
}

type stopPayload struct {
	AccountSID string `json:"accountSid"`
	CallSID    string `json:"callSid"`
}

// validateStart rejects anything that is not a stream for this session from our Twilio
// account in the expected audio format. The WebSocket handshake itself is unsigned.
func (a *Adapter) validateStart(s *session, env inboundEnvelope) error {
	st := env.Start
	if st == nil {
		return errors.New("missing start payload")
	}
	if st.StreamSID == "" {
		return errors.New("missing stream sid")
	}
	if st.AccountSID != a.cfg.AccountSID {
		return errors.New("account sid mismatch")
	}
	if sid := s.providerSID(); sid != "" && st.CallSID != sid {
		return errors.New("call sid mismatch")
	}
	if got := st.CustomParameters[callIDCustomParam]; got != string(s.id) {
		return errors.New("call id custom parameter mismatch")
	}
	if st.MediaFormat.Encoding != "audio/x-mulaw" || st.MediaFormat.SampleRate != 8000 || st.MediaFormat.Channels != 1 {
		return errors.New("unexpected media format")
	}
	return nil
}

func (a *Adapter) serveStream(w http.ResponseWriter, r *http.Request, s *session) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		a.log.Warn("twilio: websocket accept failed", "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxWebSocketMessage)
	a.log.Debug("twilio: media stream websocket accepted", "call_id", s.id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	sc := newStreamConn(s, conn)
	defer s.detachStream(sc)

	for {
		var env inboundEnvelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			if !s.isEnded() && ctx.Err() == nil {
				a.log.Debug("twilio: stream read ended", "error", err)
			}
			return
		}

		switch env.Event {
		case "connected":
			// Protocol handshake; nothing to do.
		case "start":
			if err := a.validateStart(s, env); err != nil {
				a.log.Warn("twilio: rejected stream start", "error", err, "stream_sid", env.StreamSID)
				_ = conn.Close(websocket.StatusPolicyViolation, "invalid stream start")
				return
			}
			sc.setStreamSID(env.Start.StreamSID)
			a.log.Debug("twilio: media stream started", "call_id", s.id, "stream_sid", env.Start.StreamSID)
			s.attachStream(sc)
		case "media":
			if env.Media == nil || env.Media.Track != "inbound" {
				continue
			}
			payload, err := base64.StdEncoding.DecodeString(env.Media.Payload)
			if err != nil {
				a.log.Debug("twilio: malformed media payload", "error", err)
				continue
			}
			if len(payload) > maxMediaPayload {
				a.log.Debug("twilio: oversized media payload", "bytes", len(payload))
				continue
			}
			for _, chunk := range sc.dec.decode(payload) {
				s.pushInbound(domain.AudioChunk{Data: chunk})
			}
		case "dtmf":
			if env.DTMF != nil {
				s.handleDTMF(env.DTMF.Digit)
			}
		case "mark":
			if env.Mark != nil {
				s.handleMark(env.Mark.Name)
			}
		case "stop":
			return
		default:
			a.log.Debug("twilio: unknown stream event", "event", env.Event)
		}
	}
}
