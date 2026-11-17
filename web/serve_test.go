//go:build !webembed

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPlaceholderHandlerWhenNotBuilt(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "web app not built") {
		t.Errorf("placeholder body = %q", rec.Body.String())
	}
}

func TestSPAHandlerServesFilesAndFallsBackToIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><h1>app</h1>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	handler := spaHandler(fsys)

	cases := []struct {
		path        string
		wantStatus  int
		wantBody    string
		wantCTParts string
	}{
		{"/", http.StatusOK, "<h1>app</h1>", "text/html"},
		{"/dashboard?order=abc", http.StatusOK, "<h1>app</h1>", "text/html"},
		{"/assets/app.js", http.StatusOK, "console.log('app')", "javascript"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCTParts) {
				t.Errorf("content-type = %q, want it to contain %q", ct, tc.wantCTParts)
			}
		})
	}
}
