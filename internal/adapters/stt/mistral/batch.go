package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	maxBatchInputBytes    = 64 << 20 // 64 MiB ≈ 34 minutes of canonical PCM
	maxBatchResponseBytes = 16 << 20 // 16 MiB JSON response
)

// batchResponse is the JSON Mistral returns from /v1/audio/transcriptions. Unknown fields
// are ignored for forward compatibility.
type batchResponse struct {
	Model    string `json:"model"`
	Text     string `json:"text"`
	Language string `json:"language"`
	Segments []struct {
		Type      string  `json:"type"`
		Text      string  `json:"text"`
		Start     float64 `json:"start"`
		End       float64 `json:"end"`
		SpeakerID *string `json:"speaker_id"`
	} `json:"segments"`
}

// transcribeFile sends one multipart POST with an in-memory WAV wrapping of the raw
// canonical PCM input, then maps the response to a domain.Transcript.
func (a *Adapter) transcribeFile(ctx context.Context, r io.Reader, opts ports.BatchOptions) (domain.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	pcm, err := readCapped(r, maxBatchInputBytes)
	if err != nil {
		return domain.Transcript{}, err
	}
	if len(pcm) == 0 {
		return domain.Transcript{}, errors.New("mistral stt: empty audio input")
	}
	if len(pcm)%2 != 0 {
		return domain.Transcript{}, errors.New("mistral stt: odd byte count (not PCM16)")
	}
	lang, err := normalizeLanguage(opts.Language)
	if err != nil {
		return domain.Transcript{}, err
	}
	hints := make([]string, 0, len(opts.Hints))
	for _, h := range opts.Hints {
		if strings.TrimSpace(h) == "" {
			return domain.Transcript{}, errors.New("mistral stt: empty context bias hint")
		}
		hints = append(hints, h)
	}

	body, contentType, err := buildBatchBody(a.cfg.BatchModel, lang, hints, opts, pcm)
	if err != nil {
		return domain.Transcript{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+batchTranscriptPath, body)
	if err != nil {
		return domain.Transcript{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	req.Header.Set("Content-Type", contentType)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("mistral stt: batch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.Transcript{}, decodeAPIError(resp)
	}

	respBody, err := readCapped(resp.Body, maxBatchResponseBytes)
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("mistral stt: read batch response: %w", err)
	}
	var out batchResponse
	if err := decodeSingleJSON(respBody, &out); err != nil {
		return domain.Transcript{}, fmt.Errorf("mistral stt: decode batch response: %w", err)
	}
	return mapTranscript(out, opts.Timestamps || opts.Diarization)
}

// decodeSingleJSON decodes exactly one top-level JSON value, rejecting trailing data but
// ignoring unknown fields (forward compatibility).
func decodeSingleJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// buildBatchBody assembles the multipart/form-data body for batch transcription.
func buildBatchBody(model, lang string, hints []string, opts ports.BatchOptions, pcm []byte) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="recording.wav"`)
	h.Set("Content-Type", "audio/wav")
	fw, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(wavHeader(len(pcm))); err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(pcm); err != nil {
		return nil, "", err
	}

	if err := w.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if lang != "" {
		if err := w.WriteField("language", lang); err != nil {
			return nil, "", err
		}
	}
	for _, hint := range hints {
		if err := w.WriteField("context_bias", hint); err != nil {
			return nil, "", err
		}
	}
	if opts.Timestamps || opts.Diarization {
		if err := w.WriteField("timestamp_granularities", "segment"); err != nil {
			return nil, "", err
		}
	}
	if opts.Diarization {
		if err := w.WriteField("diarize", "true"); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// mapTranscript converts the Mistral segment response into the domain shape. Segments are
// always non-nil; floating seconds become time.Duration with validation.
func mapTranscript(resp batchResponse, withSegments bool) (domain.Transcript, error) {
	tr := domain.Transcript{Text: resp.Text, Segments: []domain.TranscriptSegment{}}
	for _, seg := range resp.Segments {
		start, err := secondsToDuration(seg.Start)
		if err != nil {
			return domain.Transcript{}, err
		}
		end, err := secondsToDuration(seg.End)
		if err != nil {
			return domain.Transcript{}, err
		}
		if end < start {
			return domain.Transcript{}, errors.New("mistral stt: segment end precedes start")
		}
		speaker := ""
		if seg.SpeakerID != nil {
			speaker = *seg.SpeakerID
		}
		tr.Segments = append(tr.Segments, domain.TranscriptSegment{
			Start:   start,
			End:     end,
			Text:    seg.Text,
			Speaker: speaker,
		})
	}
	if !withSegments {
		tr.Segments = []domain.TranscriptSegment{}
	}
	return tr, nil
}

func secondsToDuration(seconds float64) (time.Duration, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, errors.New("mistral stt: invalid segment time")
	}
	return time.Duration(math.Round(seconds * float64(time.Second))), nil
}

// readCapped reads up to cap+1 bytes to detect overflow without unbounded memory.
func readCapped(r io.Reader, cap int) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, int64(cap)+1))
	if err != nil {
		return nil, fmt.Errorf("mistral stt: read audio: %w", err)
	}
	if n > int64(cap) {
		return nil, fmt.Errorf("mistral stt: audio exceeds %d bytes", cap)
	}
	return buf.Bytes(), nil
}
