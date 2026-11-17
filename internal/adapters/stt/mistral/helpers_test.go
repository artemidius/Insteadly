package mistral

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
)

const testAPIKey = "test-mistral-api-key"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{
		APIKey:     testAPIKey,
		BaseURL:    baseURL,
		HTTPClient: &http.Client{},
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// errReader returns err after any amount of data.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
