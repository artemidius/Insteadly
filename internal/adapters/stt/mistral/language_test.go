package mistral

import "testing"

func TestNormalizeLanguage(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"de", "de", "de", false},
		{"uppercase", "DE", "de", false},
		{"de-AT", "de-AT", "de", false},
		{"en-US", "en-US", "en", false},
		{"one letter", "d", "", true},
		{"three letters", "deu", "", true},
		{"non-ascii", "dé", "", true},
		{"digits", "12", "", true},
		{"empty primary with region", "-AT", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeLanguage(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeLanguage(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeLanguage(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeLanguage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
