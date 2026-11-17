package telnyx

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

// streamConn is one media-stream WebSocket. Telnyx allows one stream per call; the
// connection is the server side (Telnyx dials the stream_url we requested).
type streamConn struct {
	session *session
	conn    *websocket.Conn

	mu       sync.Mutex
	streamID string
	dec      frameDecoder
	sendMu   sync.Mutex
}

func newStreamConn(s *session, c *websocket.Conn) *streamConn {
	return &streamConn{session: s, conn: c}
}

func (c *streamConn) setStreamID(id string) {
	c.mu.Lock()
	c.streamID = id
	c.mu.Unlock()
}

func (c *streamConn) streamIDValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamID
}

func (c *streamConn) close() {
	_ = c.conn.CloseNow()
}

// writeMedia sends one µ-law frame to Telnyx. The bidirectional stream was started with
// stream_bidirectional_mode=rtp / codec PCMU, so the payload is raw µ-law RTP data.
func (c *streamConn) writeMedia(payload []byte) error {
	var msg outboundMedia
	msg.Event = "media"
	msg.Media.Payload = base64.StdEncoding.EncodeToString(payload)
	return c.write(&msg)
}

// writeMark sends one outbound playback mark. Telnyx echoes it after all previously
// queued media has played.
func (c *streamConn) writeMark(name string) error {
	var msg outboundMark
	msg.Event = "mark"
	msg.Mark.Name = name
	return c.write(&msg)
}

// writeClear clears the provider-side playback queue.
func (c *streamConn) writeClear() error {
	var msg outboundClear
	msg.Event = "clear"
	return c.write(&msg)
}

// write serializes one outbound JSON message. Telnyx outbound media/mark/clear messages
// carry no stream id.
func (c *streamConn) write(v any) error {
	if c.streamIDValue() == "" {
		return errors.New("stream has not started")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, c.conn, v)
}

type outboundMedia struct {
	Event string `json:"event"`
	Media struct {
		Payload string `json:"payload"`
	} `json:"media"`
}

type outboundMark struct {
	Event string `json:"event"`
	Mark  struct {
		Name string `json:"name"`
	} `json:"mark"`
}

type outboundClear struct {
	Event string `json:"event"`
}

// inboundEnvelope is the verified media-stream message shape: a top-level event and
// stream_id, with per-event payload objects.
type inboundEnvelope struct {
	Event          string        `json:"event"`
	StreamID       string        `json:"stream_id"`
	SequenceNumber string        `json:"sequence_number"`
	Start          *startPayload `json:"start"`
	Media          *mediaPayload `json:"media"`
	DTMF           *dtmfPayload  `json:"dtmf"`
	Mark           *markPayload  `json:"mark"`
	Stop           *stopPayload  `json:"stop"`
	Payload        *errorPayload `json:"payload"`
}

type startPayload struct {
	CallControlID string      `json:"call_control_id"`
	CallSessionID string      `json:"call_session_id"`
	From          string      `json:"from"`
	To            string      `json:"to"`
	MediaFormat   mediaFormat `json:"media_format"`
}

type mediaFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type mediaPayload struct {
	Track     string `json:"track"`
	Chunk     string `json:"chunk"`
	Timestamp string `json:"timestamp"`
	Payload   string `json:"payload"`
}

type dtmfPayload struct {
	Digit string `json:"digit"`
}

type markPayload struct {
	Name string `json:"name"`
}

type stopPayload struct {
	CallControlID string `json:"call_control_id"`
}

type errorPayload struct {
	Code   int    `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// validateStart rejects anything that is not a PCMU/8 kHz/mono stream for this session
// from the account's own connection.
func (a *Adapter) validateStart(s *session, env inboundEnvelope) error {
	st := env.Start
	if st == nil {
		return errors.New("missing start payload")
	}
	if env.StreamID == "" {
		return errors.New("missing stream id")
	}
	if st.CallControlID == "" {
		return errors.New("missing call control id")
	}
	if sid := s.providerSID(); sid != "" && st.CallControlID != sid {
		return errors.New("call control id mismatch")
	}
	if st.MediaFormat.Encoding != "PCMU" || st.MediaFormat.SampleRate != 8000 || st.MediaFormat.Channels != 1 {
		return errors.New("unexpected media format")
	}
	return nil
}

func (a *Adapter) serveStream(w http.ResponseWriter, r *http.Request, s *session) {
	// The handshake itself is signed by Telnyx over `{timestamp}|{stream_url}`. Without
	// a configured key the unguessable per-call path remains the capability check.
	if err := a.verifyStreamHandshake(r.Header, a.streamURL(s.id)); err != nil {
		a.log.Warn("telnyx: rejected media stream handshake", "call_id", s.id, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		a.log.Warn("telnyx: websocket accept failed", "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxWebSocketMessage)
	a.log.Debug("telnyx: media stream websocket accepted", "call_id", s.id)

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
				a.log.Debug("telnyx: stream read ended", "error", err)
			}
			return
		}

		switch env.Event {
		case "connected":
			// Protocol handshake; nothing to do.
		case "start":
			if err := a.validateStart(s, env); err != nil {
				a.log.Warn("telnyx: rejected stream start", "error", err, "stream_id", env.StreamID)
				_ = conn.Close(websocket.StatusPolicyViolation, "invalid stream start")
				return
			}
			sc.setStreamID(env.StreamID)
			a.log.Debug("telnyx: media stream started", "call_id", s.id, "stream_id", env.StreamID)
			s.attachStream(sc)
		case "media":
			if env.Media == nil {
				continue
			}
			// both_tracks delivers inbound and outbound frames; only the far end is
			// captured. An empty track is treated as inbound.
			if env.Media.Track != "" && env.Media.Track != "inbound" {
				continue
			}
			payload, err := base64.StdEncoding.DecodeString(env.Media.Payload)
			if err != nil {
				a.log.Debug("telnyx: malformed media payload", "error", err)
				continue
			}
			if len(payload) > maxMediaPayload {
				a.log.Debug("telnyx: oversized media payload", "bytes", len(payload))
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
		case "error":
			if env.Payload != nil {
				a.log.Warn("telnyx: stream error", "code", env.Payload.Code, "title", env.Payload.Title)
			}
		case "stop":
			// The stream ended. The call may still be alive; a later answered/reconnect
			// can attach a new stream. Do not end the call here.
			return
		default:
			a.log.Debug("telnyx: unknown stream event", "event", env.Event)
		}
	}
}
