package mistral

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/ports"
)

type capturedBatch struct {
	auth      string
	ct        string
	fields    map[string][]string
	fileBytes []byte
}

func captureBatch(t *testing.T, r *http.Request) capturedBatch {
	t.Helper()
	c := capturedBatch{auth: r.Header.Get("Authorization"), ct: r.Header.Get("Content-Type"), fields: map[string][]string{}}
	mr, err := r.MultipartReader()
	if err != nil {
		t.Fatalf("multipart reader: %v", err)
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		data, _ := io.ReadAll(p)
		if p.FormName() == "file" {
			c.fileBytes = data
		} else {
			c.fields[p.FormName()] = append(c.fields[p.FormName()], string(data))
		}
	}
	return c
}

func batchServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Adapter, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return newTestAdapter(t, ts.URL), ts
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestTranscribeFileMultipart(t *testing.T) {
	pcm := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(i))
	}
	var got capturedBatch
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != batchTranscriptPath {
			t.Errorf("path = %s, want %s", r.URL.Path, batchTranscriptPath)
		}
		got = captureBatch(t, r)
		writeJSONResp(w, http.StatusOK, batchResponse{Text: "ok", Model: "m"})
	})

	tr, err := a.TranscribeFile(context.Background(), strings.NewReader(string(pcm)), ports.BatchOptions{
		Language:    "de-AT",
		Hints:       []string{"Ordination Dr. Muster", "Hietzing"},
		Timestamps:  true,
		Diarization: true,
	})
	if err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if tr.Text != "ok" {
		t.Errorf("text = %q, want ok", tr.Text)
	}

	if got.auth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q", got.auth)
	}
	if !strings.HasPrefix(got.ct, "multipart/form-data") {
		t.Errorf("content-type = %q", got.ct)
	}
	if len(got.fileBytes) != wavHeaderBytes+len(pcm) {
		t.Fatalf("file bytes = %d, want %d", len(got.fileBytes), wavHeaderBytes+len(pcm))
	}
	if string(got.fileBytes[0:4]) != "RIFF" {
		t.Errorf("file does not start with WAV header")
	}
	if string(got.fileBytes[wavHeaderBytes:]) != string(pcm) {
		t.Error("PCM payload not byte-identical")
	}
	if got.fields["model"][0] != defaultBatchModel {
		t.Errorf("model = %q, want %q", got.fields["model"], defaultBatchModel)
	}
	if got.fields["language"][0] != "de" {
		t.Errorf("language = %q, want de", got.fields["language"])
	}
	if len(got.fields["context_bias"]) != 2 || got.fields["context_bias"][0] != "Ordination Dr. Muster" {
		t.Errorf("context_bias = %v", got.fields["context_bias"])
	}
	if got.fields["timestamp_granularities"][0] != "segment" {
		t.Errorf("timestamp_granularities = %v", got.fields["timestamp_granularities"])
	}
	if got.fields["diarize"][0] != "true" {
		t.Errorf("diarize = %v", got.fields["diarize"])
	}
}

func TestTranscribeFileTimestampsImplySegment(t *testing.T) {
	var got capturedBatch
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = captureBatch(t, r)
		writeJSONResp(w, http.StatusOK, batchResponse{Text: "x"})
	})
	if _, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{Timestamps: true}); err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if _, ok := got.fields["timestamp_granularities"]; !ok {
		t.Error("timestamps must imply timestamp_granularities=segment")
	}
	if _, ok := got.fields["diarize"]; ok {
		t.Error("diarize must be absent unless requested")
	}
}

func TestTranscribeFileResponseMapping(t *testing.T) {
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"model": "voxtral-mini-transcribe-3",
			"text":  "Guten Tag. Ich brauche einen Termin.",
			"segments": []map[string]any{
				{"type": "transcription_segment", "text": "Guten Tag.", "start": 0.0, "end": 1.25, "speaker_id": "speaker_1"},
				{"type": "transcription_segment", "text": " Ich brauche einen Termin.", "start": 1.4, "end": 2.8, "speaker_id": nil},
			},
		})
	})
	tr, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{Timestamps: true})
	if err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if tr.Text != "Guten Tag. Ich brauche einen Termin." {
		t.Errorf("text = %q", tr.Text)
	}
	if len(tr.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(tr.Segments))
	}
	if tr.Segments[0].Start != 0 || tr.Segments[0].End != 1250*time.Millisecond {
		t.Errorf("segment 0 times = %v..%v", tr.Segments[0].Start, tr.Segments[0].End)
	}
	if tr.Segments[0].Speaker != "speaker_1" {
		t.Errorf("segment 0 speaker = %q", tr.Segments[0].Speaker)
	}
	if tr.Segments[1].Speaker != "" {
		t.Errorf("segment 1 speaker = %q, want empty", tr.Segments[1].Speaker)
	}
	if tr.Segments[1].End < tr.Segments[1].Start {
		t.Error("segment 1 reversed")
	}
}

func TestTranscribeFileNoTimestampsEmptySegments(t *testing.T) {
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"text":     "hi",
			"segments": []map[string]any{{"text": "hi", "start": 0.0, "end": 1.0}},
		})
	})
	tr, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{})
	if err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if tr.Segments == nil {
		t.Fatal("Segments must be non-nil")
	}
	if len(tr.Segments) != 0 {
		t.Errorf("Segments = %v, want empty", tr.Segments)
	}
}

func TestTranscribeFileRejectsBadSegments(t *testing.T) {
	cases := []map[string]any{
		{"text": "x", "segments": []map[string]any{{"start": -0.1, "end": 1.0}}},
		{"text": "x", "segments": []map[string]any{{"start": 2.0, "end": 1.0}}},
	}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSONResp(w, http.StatusOK, body)
			})
			_, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{Timestamps: true})
			if err == nil {
				t.Fatal("expected error for malformed segment")
			}
		})
	}
}

func TestTranscribeFileInputValidation(t *testing.T) {
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request must not be made for invalid input")
	})

	if _, err := a.TranscribeFile(context.Background(), strings.NewReader(""), ports.BatchOptions{}); err == nil {
		t.Error("empty input must error")
	}
	if _, err := a.TranscribeFile(context.Background(), strings.NewReader("abc"), ports.BatchOptions{}); err == nil {
		t.Error("odd input must error")
	}
	if _, err := a.TranscribeFile(context.Background(), errReader{err: io.ErrUnexpectedEOF}, ports.BatchOptions{}); err == nil {
		t.Error("reader error must propagate")
	}
	if _, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 4))), ports.BatchOptions{Language: "d"}); err == nil {
		t.Error("malformed language must error before request")
	}
}

func TestTranscribeFileOverflow(t *testing.T) {
	// readCapped is exercised directly so the test need not allocate 64 MiB.
	if _, err := readCapped(strings.NewReader(strings.Repeat("a", 5)), 4); err == nil {
		t.Error("expected overflow error")
	}
	data, err := readCapped(strings.NewReader(strings.Repeat("a", 4)), 4)
	if err != nil || len(data) != 4 {
		t.Fatalf("readCapped under cap: len=%d err=%v", len(data), err)
	}
}

func TestTranscribeFileAPIErrors(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		wantStatus int
		wantCode   string
		temporary  bool
		retryAfter string
		wantDelay  time.Duration
	}{
		{400, `{"object":"error","type":"invalid_request_error","code":"bad","param":"model","message":"secret-ish"}`, 400, "bad", false, "", 0},
		{401, `{"object":"error","message":"bad key"}`, 401, "", false, "", 0},
		{402, `{}`, 402, "", false, "", 0},
		{404, `{"code":42}`, 404, "42", false, "", 0},
		{422, `{}`, 422, "", false, "", 0},
		{429, `{"code":"rate_limited"}`, 429, "rate_limited", true, "3", 3 * time.Second},
		{500, `{}`, 500, "", true, "", 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %T %v, want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tc.wantStatus)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if apiErr.Temporary() != tc.temporary {
				t.Errorf("temporary = %v, want %v", apiErr.Temporary(), tc.temporary)
			}
			if apiErr.RetryDelay() != tc.wantDelay {
				t.Errorf("retry delay = %v, want %v", apiErr.RetryDelay(), tc.wantDelay)
			}
			if strings.Contains(err.Error(), "secret-ish") || strings.Contains(err.Error(), "bad key") {
				t.Errorf("error leaks message text: %v", err)
			}
		})
	}
}

func TestTranscribeFileContextCancelled(t *testing.T) {
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request must not be made when context is cancelled")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.TranscribeFile(ctx, strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestTranscribeFileRejectsTrailingJSON(t *testing.T) {
	a, _ := batchServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusOK, map[string]any{"text": "hi"})
		_, _ = io.WriteString(w, `{"extra":1}`)
	})
	if _, err := a.TranscribeFile(context.Background(), strings.NewReader(string(make([]byte, 640))), ports.BatchOptions{}); err == nil {
		t.Fatal("trailing JSON after the top-level object must error")
	}
}

func TestAdapterNewValidation(t *testing.T) {
	if _, err := New(Config{APIKey: ""}); err == nil {
		t.Error("empty api key must error")
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "http://api.mistral.ai"}); err == nil {
		t.Error("non-loopback http base url must error")
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "http://localhost:1234"}); err != nil {
		t.Errorf("loopback http should be allowed: %v", err)
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "ftp://x"}); err == nil {
		t.Error("ftp scheme must error")
	}
}

var _ ports.SpeechToText = (*Adapter)(nil)
