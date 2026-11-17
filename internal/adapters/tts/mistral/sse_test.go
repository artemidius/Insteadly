package mistral

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSEEventBasics(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("event: speech.audio.delta\ndata: {\"a\":1}\n\n"))
	data, event, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if data == nil || string(data) != `{"a":1}` {
		t.Errorf("data = %q", data)
	}
	if event != "speech.audio.delta" {
		t.Errorf("event = %q", event)
	}
}

func TestReadSSEEventCRLFCommentMultiline(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(":keepalive\r\ndata: line1\r\ndata: line2\r\n\r\n"))
	data, _, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if string(data) != "line1\nline2" {
		t.Errorf("data = %q, want %q", data, "line1\nline2")
	}
}

func TestReadSSEEventTrailingUnterminatedLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`data: {"a":1}`))
	data, _, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("data = %q", data)
	}
	_, _, err = readSSEEvent(br, maxSSEEventBytes)
	if !errors.Is(err, io.EOF) {
		t.Errorf("second read = %v, want io.EOF", err)
	}
}

func TestReadSSEEventOversizedLine(t *testing.T) {
	// A newline-free line larger than the cap must error rather than allocate unbounded.
	br := bufio.NewReader(strings.NewReader(strings.Repeat("a", 1000)))
	_, _, err := readSSEEvent(br, 100)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestReadSSEEventLongLineWithinCap(t *testing.T) {
	// A line longer than the bufio buffer (4096) but within the cap must parse correctly,
	// exercising the multi-fragment accumulation path.
	line := "data: " + strings.Repeat("b", 10000) + "\n\n"
	br := bufio.NewReader(strings.NewReader(line))
	data, _, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if len(data) != 10000 {
		t.Errorf("data length = %d, want 10000", len(data))
	}
}

func TestReadSSEEventEventCrossCheckField(t *testing.T) {
	// Multiple event:/data: lines: the last event name wins; data is joined.
	br := bufio.NewReader(strings.NewReader("event: a\ndata: x\nevent: b\ndata: y\n\n"))
	data, event, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if string(data) != "x\ny" || event != "b" {
		t.Errorf("data=%q event=%q", data, event)
	}
}
