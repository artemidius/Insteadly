package mistral

import (
	"encoding/binary"
	"testing"
)

func TestWAVHeader(t *testing.T) {
	h := wavHeader(640)
	if len(h) != wavHeaderBytes {
		t.Fatalf("header length = %d, want %d", len(h), wavHeaderBytes)
	}
	if string(h[0:4]) != "RIFF" {
		t.Errorf("RIFF = %q", h[0:4])
	}
	if got := binary.LittleEndian.Uint32(h[4:8]); got != 36+640 {
		t.Errorf("RIFF size = %d, want %d", got, 36+640)
	}
	if string(h[8:12]) != "WAVE" {
		t.Errorf("WAVE = %q", h[8:12])
	}
	if string(h[12:16]) != "fmt " {
		t.Errorf("fmt = %q", h[12:16])
	}
	if got := binary.LittleEndian.Uint32(h[16:20]); got != 16 {
		t.Errorf("fmt size = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint16(h[20:22]); got != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(h[22:24]); got != 1 {
		t.Errorf("channels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(h[24:28]); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := binary.LittleEndian.Uint32(h[28:32]); got != 32000 {
		t.Errorf("byte rate = %d, want 32000", got)
	}
	if got := binary.LittleEndian.Uint16(h[32:34]); got != 2 {
		t.Errorf("block align = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(h[34:36]); got != 16 {
		t.Errorf("bits/sample = %d, want 16", got)
	}
	if string(h[36:40]) != "data" {
		t.Errorf("data = %q", h[36:40])
	}
	if got := binary.LittleEndian.Uint32(h[40:44]); got != 640 {
		t.Errorf("data size = %d, want 640", got)
	}
}

func TestWAVHeaderZero(t *testing.T) {
	h := wavHeader(0)
	if got := binary.LittleEndian.Uint32(h[4:8]); got != 36 {
		t.Errorf("RIFF size = %d, want 36", got)
	}
	if got := binary.LittleEndian.Uint32(h[40:44]); got != 0 {
		t.Errorf("data size = %d, want 0", got)
	}
}
