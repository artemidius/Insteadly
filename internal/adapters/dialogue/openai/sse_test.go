package openai

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSEEventBasics(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("data: {\"a\":1}\n\n"))
	data, _, err := readSSEEvent(br, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if data == nil || string(data) != `{"a":1}` {
		t.Errorf("data = %q", data)
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
	br := bufio.NewReader(strings.NewReader(strings.Repeat("a", 1000)))
	_, _, err := readSSEEvent(br, 100)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestReadSSEEventLongLineWithinCap(t *testing.T) {
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
