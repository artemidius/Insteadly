package recorder

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWAVHeader(t *testing.T) {
	pcm := make([]byte, 640*3)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	wav := wavBytes(pcm)

	if len(wav) != wavHeaderSize+len(pcm) {
		t.Fatalf("len = %d, want %d", len(wav), wavHeaderSize+len(pcm))
	}
	if got := string(wav[0:4]); got != "RIFF" {
		t.Errorf("RIFF = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+len(pcm)) {
		t.Errorf("RIFF size = %d, want %d", got, 36+len(pcm))
	}
	if got := string(wav[8:12]); got != "WAVE" {
		t.Errorf("WAVE = %q", got)
	}
	if got := string(wav[12:16]); got != "fmt " {
		t.Errorf("fmt = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[16:20]); got != 16 {
		t.Errorf("fmt size = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Errorf("audio format = %d, want PCM (1)", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels = %d, want mono", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := binary.LittleEndian.Uint32(wav[28:32]); got != 32000 {
		t.Errorf("byte rate = %d, want 32000", got)
	}
	if got := binary.LittleEndian.Uint16(wav[32:34]); got != 2 {
		t.Errorf("block align = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if got := string(wav[36:40]); got != "data" {
		t.Errorf("data = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data size = %d, want %d", got, len(pcm))
	}
	if !bytes.Equal(wav[wavHeaderSize:], pcm) {
		t.Fatal("PCM payload is not byte-identical after the header")
	}
}

func TestWAVRoundTrip(t *testing.T) {
	pcm := []byte{0x00, 0x00, 0xff, 0x7f, 0x00, 0x80, 0x01, 0x02}
	wav := wavBytes(pcm)
	if !bytes.Equal(wav[wavHeaderSize:], pcm) {
		t.Fatal("round trip differs")
	}
}

func TestWAVOddLengthDropsTrailingByte(t *testing.T) {
	wav := wavBytes([]byte{1, 2, 3})
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 2 {
		t.Fatalf("data size = %d, want 2 (odd byte dropped)", got)
	}
	if len(wav) != wavHeaderSize+2 {
		t.Fatalf("len = %d", len(wav))
	}
}

func TestWAVEmpty(t *testing.T) {
	wav := wavBytes(nil)
	if len(wav) != wavHeaderSize {
		t.Fatalf("len = %d, want a bare header", len(wav))
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 0 {
		t.Fatalf("data size = %d, want 0", got)
	}
}
