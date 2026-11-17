package recorder

import (
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func boolPtr(v bool) *bool { return &v }

func TestResolveGlobalOnly(t *testing.T) {
	cases := []struct {
		name                 string
		global               Policy
		wantCapture, wantWAV bool
		wantTranscript       bool
	}{
		{"all off", Policy{}, false, false, false},
		{"recording only", Policy{Recording: true}, false, false, false},
		{"audio only", Policy{Recording: true, Audio: true}, true, true, false},
		{"transcript only", Policy{Recording: true, Transcript: true}, true, false, true},
		{"both", Policy{Recording: true, Audio: true, Transcript: true}, true, true, true},
		{"audio without master gate", Policy{Audio: true, Transcript: true}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.global, nil)
			if got.Capture != tc.wantCapture || got.WAV != tc.wantWAV || got.Transcript != tc.wantTranscript {
				t.Fatalf("Resolve(%+v) = %+v, want capture=%v wav=%v transcript=%v",
					tc.global, got, tc.wantCapture, tc.wantWAV, tc.wantTranscript)
			}
		})
	}
}

// TestResolvePerOrderMatrix is the §7.4 matrix, exact.
func TestResolvePerOrderMatrix(t *testing.T) {
	cases := []struct {
		name           string
		global         Policy
		overrides      *domain.RecordingFlags
		wantCapture    bool
		wantWAV        bool
		wantTranscript bool
		wantAll        bool
	}{
		{
			name:    "global off, no override",
			global:  Policy{},
			wantAll: false,
		},
		{
			name:        "global WAV only",
			global:      Policy{Recording: true, Audio: true},
			wantCapture: true, wantWAV: true,
		},
		{
			name:        "global transcript only",
			global:      Policy{Recording: true, Transcript: true},
			wantCapture: true, wantTranscript: true,
		},
		{
			name:        "global WAV + transcript",
			global:      Policy{Recording: true, Audio: true, Transcript: true},
			wantCapture: true, wantWAV: true, wantTranscript: true,
		},
		{
			name:   "global off overridden on",
			global: Policy{},
			overrides: &domain.RecordingFlags{
				Recording: boolPtr(true), Audio: boolPtr(true),
			},
			wantCapture: true, wantWAV: true,
		},
		{
			name:        "field-level audio on",
			global:      Policy{Recording: true},
			overrides:   &domain.RecordingFlags{Audio: boolPtr(true)},
			wantCapture: true, wantWAV: true,
		},
		{
			name:        "field-level audio off keeps transcript",
			global:      Policy{Recording: true, Audio: true, Transcript: true},
			overrides:   &domain.RecordingFlags{Audio: boolPtr(false)},
			wantCapture: true, wantTranscript: true,
		},
		{
			name:      "master gate off wins over artifact on",
			global:    Policy{Recording: true, Audio: true, Transcript: true},
			overrides: &domain.RecordingFlags{Recording: boolPtr(false)},
			wantAll:   false,
		},
		{
			name:        "nil override pointers fall through",
			global:      Policy{Recording: true, Audio: true},
			overrides:   &domain.RecordingFlags{},
			wantCapture: true, wantWAV: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.global, tc.overrides)
			wantCapture := tc.wantCapture && !tc.wantAll
			wantWAV := tc.wantWAV && !tc.wantAll
			wantTranscript := tc.wantTranscript && !tc.wantAll
			if got.Capture != wantCapture || got.WAV != wantWAV || got.Transcript != wantTranscript {
				t.Fatalf("Resolve = %+v, want capture=%v wav=%v transcript=%v",
					got, wantCapture, wantWAV, wantTranscript)
			}
			if tc.wantAll {
				if got.Capture || got.WAV || got.Transcript {
					t.Fatalf("master gate off must disable everything: %+v", got)
				}
			}
		})
	}
}
