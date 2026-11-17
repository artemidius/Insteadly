package telnyx

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"testing"
	"time"
)

func TestParsePublicKeyBase64AndPEM(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	rawB64 := base64.StdEncoding.EncodeToString(pub)
	got, err := parseEd25519PublicKey(rawB64)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("base64 key: got %v err %v", got, err)
	}

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	got, err = parseEd25519PublicKey(pemKey)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("PEM key: got %v err %v", got, err)
	}

	for _, bad := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := parseEd25519PublicKey(bad); err == nil {
			t.Errorf("accepted invalid key %q", bad)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Unix(1_800_000_000, 0)
	ts := "1800000000"
	body := []byte(`{"data":{"event_type":"call.answered"}}`)
	msg := append([]byte(ts+"|"), body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := verifySignature(pub, ts, sig, body, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Hex is accepted as an alternate encoding.
	hexSig := hex.EncodeToString(ed25519.Sign(priv, msg))
	if err := verifySignature(pub, ts, hexSig, body, now); err != nil {
		t.Fatalf("hex signature rejected: %v", err)
	}
	// Tampered body.
	if err := verifySignature(pub, ts, sig, []byte(`{"data":{}}`), now); err == nil {
		t.Fatal("tampered body accepted")
	}
	// Wrong timestamp in the signed string.
	if err := verifySignature(pub, "1800000001", sig, body, now); err == nil {
		t.Fatal("wrong timestamp accepted")
	}
	// Stale timestamp (replay).
	if err := verifySignature(pub, ts, sig, body, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	// Missing key / timestamp.
	if err := verifySignature(nil, ts, sig, body, now); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := verifySignature(pub, "", sig, body, now); err == nil {
		t.Fatal("missing timestamp accepted")
	}
	// Garbage signature.
	if err := verifySignature(pub, ts, "garbage", body, now); err == nil {
		t.Fatal("garbage signature accepted")
	}
}

func TestVerifySignedRequestPolicy(t *testing.T) {
	_, _, a := newSigningKey(t)
	if len(a.publicKey) == 0 {
		t.Fatal("adapter did not parse the configured key")
	}
	h := http.Header{}
	// No signing headers at all: capability check path.
	if err := a.verifySignedRequest(h, []byte("{}")); err != nil {
		t.Fatalf("unsigned request rejected: %v", err)
	}
	h.Set(signatureHeader, "abc")
	if err := a.verifySignedRequest(h, []byte("{}")); err == nil {
		t.Fatal("signature without timestamp accepted")
	}
}
