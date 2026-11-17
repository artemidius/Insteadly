package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func pcmFrame(amplitude int16) []byte {
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(amplitude))
	}
	return frame
}

func testGate(maxUtterance int) *gate {
	return newGate(gateConfig{
		speechThreshold:    defaultSpeechThreshold,
		preRollFrames:      defaultPreRollFrames,
		startFrames:        defaultStartFrames,
		minActiveFrames:    defaultMinActiveFrames,
		endSilenceFrames:   defaultEndSilenceFrames,
		maxUtteranceFrames: maxUtterance,
		listenTimeout:      defaultListenTimeout,
	})
}

func inboundOf(chunks [][]byte) chan domain.AudioChunk {
	ch := make(chan domain.AudioChunk, len(chunks)+1)
	for _, c := range chunks {
		ch <- domain.AudioChunk{Data: c}
	}
	return ch
}

func TestFrameRMSKnownVectors(t *testing.T) {
	if got := frameRMS(silenceFrame()); got != 0 {
		t.Errorf("silence RMS = %v", got)
	}
	if got := frameRMS(pcmFrame(1000)); got != 1000 {
		t.Errorf("constant 1000 RMS = %v", got)
	}
	if got := frameRMS(pcmFrame(-1000)); got != 1000 {
		t.Errorf("constant -1000 RMS = %v", got)
	}
	alternating := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		v := int16(1000)
		if i%2 == 1 {
			v = -1000
		}
		binary.LittleEndian.PutUint16(alternating[2*i:], uint16(v))
	}
	if got := frameRMS(alternating); got != 1000 {
		t.Errorf("alternating RMS = %v", got)
	}
}

func TestGateThresholdBoundary(t *testing.T) {
	chunks := flatten(frames(10, pcmFrame(defaultSpeechThreshold)), frames(30, silenceFrame()))
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("threshold-equal frames: %v", err)
	}
	if utt.active != 10 {
		t.Errorf("active = %d, want 10", utt.active)
	}

	below := flatten(frames(10, pcmFrame(defaultSpeechThreshold-1)), frames(30, silenceFrame()))
	g := newGate(gateConfig{
		speechThreshold:    defaultSpeechThreshold,
		preRollFrames:      defaultPreRollFrames,
		startFrames:        defaultStartFrames,
		minActiveFrames:    defaultMinActiveFrames,
		endSilenceFrames:   defaultEndSilenceFrames,
		maxUtteranceFrames: defaultMaxUtteranceFrames,
		listenTimeout:      20 * time.Millisecond,
	})
	if _, err := g.next(context.Background(), inboundOf(below), nil); !errors.Is(err, errListenTimeout) {
		t.Fatalf("below-threshold err = %v, want listen timeout", err)
	}
}

func TestGateSilenceAndPreRoll(t *testing.T) {
	// 20 leading silence frames: the pre-roll ring keeps only the last 10.
	chunks := flatten(frames(20, silenceFrame()), frames(10, pcmFrame(2000)), frames(30, silenceFrame()))
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if utt.active != 10 {
		t.Errorf("active = %d, want 10", utt.active)
	}
	// 7 silence pre-roll + 3 start frames + 7 further speech + 30 trailing silence.
	if len(utt.frames) != 47 {
		t.Fatalf("utterance frames = %d, want 47", len(utt.frames))
	}
	for i := 0; i < 7; i++ {
		if frameRMS(utt.frames[i]) != 0 {
			t.Errorf("pre-roll frame %d is not silence", i)
		}
	}
	for i := 7; i < 17; i++ {
		if frameRMS(utt.frames[i]) == 0 {
			t.Errorf("speech frame %d is silent", i)
		}
	}
	for i := 17; i < 47; i++ {
		if frameRMS(utt.frames[i]) != 0 {
			t.Errorf("trailing frame %d is not silence", i)
		}
	}
	for _, frame := range utt.frames {
		if len(frame) != domain.FrameBytes {
			t.Fatalf("frame length = %d", len(frame))
		}
	}
}

func TestGateStartPersistenceIgnoresClicks(t *testing.T) {
	// Two active frames are not enough to start; the following silence must not produce
	// an utterance. The subsequent real utterance is the first returned boundary.
	chunks := flatten(
		frames(2, pcmFrame(3000)),
		frames(10, silenceFrame()),
		frames(10, pcmFrame(3000)),
		frames(30, silenceFrame()),
	)
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if utt.active != 10 {
		t.Errorf("active = %d, want 10 (click must be discarded)", utt.active)
	}
	// 7 silence pre-roll + 3 start frames + 7 further speech + 30 trailing silence.
	if len(utt.frames) != 47 {
		t.Errorf("frames = %d, want 47", len(utt.frames))
	}
}

func TestGateMinimumUtteranceDiscardsShortNoise(t *testing.T) {
	chunks := flatten(
		frames(5, pcmFrame(3000)),
		frames(40, silenceFrame()),
		frames(10, pcmFrame(3000)),
		frames(30, silenceFrame()),
	)
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if utt.active != 10 {
		t.Errorf("active = %d, want 10 (short noise discarded)", utt.active)
	}
}

func TestGateEndSilenceRequiresThirtyFrames(t *testing.T) {
	// 29 trailing silence frames followed by more speech must not split the utterance.
	chunks := flatten(
		frames(10, pcmFrame(2000)),
		frames(29, silenceFrame()),
		frames(5, pcmFrame(2000)),
		frames(30, silenceFrame()),
	)
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if utt.active != 15 {
		t.Errorf("active = %d, want 15", utt.active)
	}
	// 3 active pre-roll (start) + 7 further speech + 29 silence + 5 speech + 30 silence.
	if len(utt.frames) != 74 {
		t.Errorf("frames = %d, want 74", len(utt.frames))
	}
}

func TestGateForcedMaximumUtterance(t *testing.T) {
	g := testGate(defaultMaxUtteranceFrames)
	chunks := flatten(frames(defaultMaxUtteranceFrames+100, pcmFrame(2000)))
	utt, err := g.next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(utt.frames) != defaultMaxUtteranceFrames {
		t.Errorf("frames = %d, want %d", len(utt.frames), defaultMaxUtteranceFrames)
	}
	if utt.active != defaultMaxUtteranceFrames {
		t.Errorf("active = %d", utt.active)
	}
}

func TestGateArbitraryChunkSizesAndResidual(t *testing.T) {
	// 7 leading silence frames fill the pre-roll exactly, so the utterance bytes equal
	// the complete input stream.
	stream := flatten(frames(7, silenceFrame()), frames(10, pcmFrame(2000)), frames(30, silenceFrame()))
	raw := bytes.Join(stream, nil)
	var chunks [][]byte
	sizes := []int{100, 540, 640, 1000, 300, 2000}
	offset := 0
	i := 0
	for offset < len(raw) {
		size := sizes[i%len(sizes)]
		if offset+size > len(raw) {
			size = len(raw) - offset
		}
		chunks = append(chunks, raw[offset:offset+size])
		offset += size
		i++
	}
	utt, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	var got []byte
	for _, frame := range utt.frames {
		got = append(got, frame...)
	}
	if len(got) != len(raw) {
		t.Fatalf("utterance bytes = %d, want %d", len(got), len(raw))
	}
	if !bytes.Equal(got, raw) {
		t.Error("reframed utterance bytes differ from the input stream")
	}
}

func TestGatePadsOneResidualFrameAtBoundary(t *testing.T) {
	stream := flatten(frames(10, pcmFrame(2000)), frames(30, silenceFrame()))
	raw := append(bytes.Join(stream, nil), make([]byte, 200)...)
	// The final chunk contains the last silence frame plus 200 residual bytes; the
	// boundary is detected on the complete frame and the residual is zero-padded once.
	chunks := [][]byte{
		raw[:39*domain.FrameBytes],
		raw[39*domain.FrameBytes:],
	}
	g := testGate(defaultMaxUtteranceFrames)
	utt, err := g.next(context.Background(), inboundOf(chunks), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(utt.frames) != len(stream)+1 {
		t.Fatalf("frames = %d, want %d (one padded residual)", len(utt.frames), len(stream)+1)
	}
	last := utt.frames[len(utt.frames)-1]
	if len(last) != domain.FrameBytes {
		t.Errorf("padded frame length = %d", len(last))
	}
	// The padded tail preserves the residual bytes and zero-fills the rest.
	tail := raw[len(raw)-200:]
	if !bytes.Equal(last[:200], tail) {
		t.Error("padded frame did not preserve residual bytes")
	}
	for _, b := range last[200:] {
		if b != 0 {
			t.Error("padded frame is not zero-filled")
			break
		}
	}
}

func TestGateMalformedChunks(t *testing.T) {
	for name, chunk := range map[string][]byte{
		"empty": nil,
		"odd":   {1, 2, 3},
	} {
		t.Run(name, func(t *testing.T) {
			ch := make(chan domain.AudioChunk, 1)
			ch <- domain.AudioChunk{Data: chunk}
			if _, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), ch, nil); err == nil {
				t.Fatal("expected malformed-chunk error")
			}
		})
	}
}

func TestGateListenTimeout(t *testing.T) {
	g := newGate(gateConfig{
		speechThreshold:    defaultSpeechThreshold,
		preRollFrames:      defaultPreRollFrames,
		startFrames:        defaultStartFrames,
		minActiveFrames:    defaultMinActiveFrames,
		endSilenceFrames:   defaultEndSilenceFrames,
		maxUtteranceFrames: defaultMaxUtteranceFrames,
		listenTimeout:      10 * time.Millisecond,
	})
	ch := make(chan domain.AudioChunk)
	start := time.Now()
	_, err := g.next(context.Background(), ch, nil)
	if !errors.Is(err, errListenTimeout) {
		t.Fatalf("err = %v, want errListenTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("listen timeout took %v", elapsed)
	}
}

func TestGateCallEndAndCancellation(t *testing.T) {
	ended := make(chan domain.CallEvent, 1)
	ended <- domain.CallEvent{Kind: domain.EventEnded, EndReason: domain.EndAborted}
	if _, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), make(chan domain.AudioChunk), ended); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("ended err = %v, want ErrCallEnded", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testGate(defaultMaxUtteranceFrames).next(ctx, make(chan domain.AudioChunk), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err = %v, want context.Canceled", err)
	}

	closed := make(chan domain.AudioChunk)
	close(closed)
	if _, err := testGate(defaultMaxUtteranceFrames).next(context.Background(), closed, nil); !errors.Is(err, errInboundClosed) {
		t.Fatalf("closed inbound err = %v, want errInboundClosed", err)
	}
}

func TestGateKeepsLeftoverFramesAcrossUtterances(t *testing.T) {
	// Both utterances arrive in one chunk; the gate must not lose the second one.
	stream := bytes.Join(flatten(
		frames(10, pcmFrame(2000)), frames(30, silenceFrame()),
		frames(10, pcmFrame(2000)), frames(30, silenceFrame()),
	), nil)
	g := testGate(defaultMaxUtteranceFrames)
	in := inboundOf([][]byte{stream})
	first, err := g.next(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("first next: %v", err)
	}
	second, err := g.next(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("second next: %v", err)
	}
	if first.active != 10 || second.active != 10 {
		t.Errorf("active counts = %d/%d", first.active, second.active)
	}
	if len(first.frames) != 40 || len(second.frames) != 40 {
		t.Errorf("frame counts = %d/%d, want 40/40", len(first.frames), len(second.frames))
	}
}
