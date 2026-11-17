package twilio

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

// Published test vector from the Twilio Go SDK's request validator tests. The auth token
// is the literal string "12345"; the URL carries query parameters, which are part of the
// signed URL rather than the form parameter set.
const (
	twilioTestToken     = "12345"
	twilioTestURL       = "https://mycompany.com/myapp.php?foo=1&bar=2"
	twilioTestSignature = "vOEb5UThFn24KEfnOFLQY2AE5FY="
)

func twilioTestParams() url.Values {
	return url.Values{
		"CallSid":               {"CA1234567890ABCDE"},
		"Caller":                {"+14158675309"},
		"Digits":                {"1234"},
		"From":                  {"+14158675309"},
		"Reason":                {"Participant"},
		"ReasonConferenceEnded": {"test"},
		"To":                    {"+18005551212"},
	}
}

func TestValidSignatureTwilioVector(t *testing.T) {
	if !validSignature(twilioTestToken, twilioTestURL, twilioTestSignature, twilioTestParams()) {
		t.Fatal("published Twilio test vector did not validate")
	}
}

func TestValidSignatureRejects(t *testing.T) {
	cases := map[string]struct {
		token     string
		url       string
		signature string
		params    url.Values
	}{
		"wrong token":    {"wrong", twilioTestURL, twilioTestSignature, twilioTestParams()},
		"tampered param": {twilioTestToken, twilioTestURL, twilioTestSignature, url.Values{"CallSid": {"CAevil"}}},
		"wrong url":      {twilioTestToken, "https://mycompany.com/other.php?foo=1&bar=2", twilioTestSignature, twilioTestParams()},
		"empty sig":      {twilioTestToken, twilioTestURL, "", twilioTestParams()},
		"empty token":    {"", twilioTestURL, twilioTestSignature, twilioTestParams()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if validSignature(tc.token, tc.url, tc.signature, tc.params) {
				t.Fatal("signature unexpectedly valid")
			}
		})
	}
}

func TestValidSignatureDefaultPortTolerance(t *testing.T) {
	withPort := "https://mycompany.com:443/myapp.php?foo=1&bar=2"
	if !validSignature(twilioTestToken, withPort, twilioTestSignature, twilioTestParams()) {
		t.Fatal("signature generated without default port should validate for URL with :443")
	}

	// A non-default port changes the signed string; the vector must not validate.
	if validSignature(twilioTestToken, "https://mycompany.com:8443/myapp.php?foo=1&bar=2", twilioTestSignature, twilioTestParams()) {
		t.Fatal("signature validated for an unrelated non-default port")
	}
}

func TestSignedURLUsesPublicBase(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/webhooks/telephony/twilio/voice/abc?x=1", nil)
	r.Host = "internal.invalid"
	got := signedURL(r, "https://abc.trycloudflare.com")
	want := "https://abc.trycloudflare.com/webhooks/telephony/twilio/voice/abc?x=1"
	if got != want {
		t.Fatalf("signedURL = %q, want %q", got, want)
	}
}
