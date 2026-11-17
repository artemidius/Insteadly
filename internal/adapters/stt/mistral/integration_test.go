package mistral

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestRealtimeIntegrationArbitraryChunks(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	// Arbitrary even-length canonical chunks (not necessarily 640 bytes).
	sizes := []int{640, 128, 320, 960, 64}
	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i)
		}
		if err := stream.Feed(context.Background(), domain.AudioChunk{Data: data}); err != nil {
			t.Fatalf("Feed(%d): %v", size, err)
		}
	}
	done := make(chan []ports.TranscriptionEvent, 1)
	go func() {
		var evs []ports.TranscriptionEvent
		for ev := range stream.Results() {
			evs = append(evs, ev)
		}
		done <- evs
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	evs := <-done
	if len(evs) == 0 || !evs[len(evs)-1].Final || evs[len(evs)-1].Text != "Guten Tag." {
		t.Fatalf("events = %+v", evs)
	}
}

func TestBatchIntegration(t *testing.T) {
	pcm := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(i))
	}
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, map[string]any{
			"text": "Hallo.",
			"segments": []map[string]any{
				{"text": "Hallo.", "start": 0.0, "end": 0.5, "speaker_id": "speaker_1"},
			},
		})
	})
	tr, err := a.TranscribeFile(context.Background(), bytes.NewReader(pcm), ports.BatchOptions{Timestamps: true})
	if err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if tr.Text != "Hallo." || len(tr.Segments) != 1 || tr.Segments[0].Speaker != "speaker_1" {
		t.Errorf("transcript = %+v", tr)
	}
}

func TestStreamGoroutinesExit(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	for i := 0; i < 5; i++ {
		stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
		if err != nil {
			t.Fatalf("TranscribeStream: %v", err)
		}
		ts := stream.(*transcriptionStream)
		go func() {
			for range stream.Results() {
			}
		}()
		if err := stream.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case <-ts.writerDone:
		default:
			t.Fatal("writer goroutine did not exit")
		}
		select {
		case <-ts.readerDone:
		default:
			t.Fatal("reader goroutine did not exit")
		}
	}
}
