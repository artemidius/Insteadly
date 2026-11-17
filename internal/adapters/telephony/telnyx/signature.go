package telnyx

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Telnyx signs webhooks and media-stream WebSocket handshakes with the account's Ed25519
// key pair:
//
//	telnyx-signature-ed25519: base64 Ed25519 signature
//	telnyx-timestamp:         Unix seconds
//
// Webhooks sign `{timestamp}|{raw body}`; the WebSocket handshake signs
// `{timestamp}|{stream_url}`. The public key is visible in the Telnyx portal
// (Account Settings -> Keys & Credentials) and configured via
// INSTEADLY_TELNYX_WEBHOOK_PUBLIC_KEY.
const (
	signatureHeader = "telnyx-signature-ed25519"
	timestampHeader = "telnyx-timestamp"
	// signatureTolerance bounds replay: reject anything older/newer than five minutes.
	signatureTolerance = 5 * time.Minute
)

// ed25519PublicKey is nil when no key is configured.
type ed25519PublicKey = ed25519.PublicKey

var (
	errNoPublicKey      = errors.New("no webhook public key configured")
	errMissingTimestamp = errors.New("missing telnyx-timestamp")
	errStaleTimestamp   = errors.New("stale telnyx-timestamp")
	errBadSignature     = errors.New("invalid ed25519 signature")
)

// parseEd25519PublicKey accepts the two shapes the Telnyx portal can give: a base64 raw
// 32-byte key, or a PEM "PUBLIC KEY" block (PKIX).
func parseEd25519PublicKey(raw string) (ed25519.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty public key")
	}
	if strings.Contains(raw, "BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return nil, errors.New("malformed PEM public key")
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PEM public key: %w", err)
		}
		ed, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("PEM key is not Ed25519")
		}
		return ed, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("public key is not base64")
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), nil
}

// verifySignature checks one signed payload against the configured public key. now is a
// parameter so tests can pin the clock.
func verifySignature(pub ed25519.PublicKey, timestamp, signature string, payload []byte, now time.Time) error {
	if len(pub) == 0 {
		return errNoPublicKey
	}
	if timestamp == "" {
		return errMissingTimestamp
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return errStaleTimestamp
	}
	if delta := now.Sub(time.Unix(ts, 0)); delta > signatureTolerance || delta < -signatureTolerance {
		return errStaleTimestamp
	}
	sig, err := decodeSignature(signature)
	if err != nil {
		return errBadSignature
	}
	message := make([]byte, 0, len(timestamp)+1+len(payload))
	message = append(message, timestamp...)
	message = append(message, '|')
	message = append(message, payload...)
	if !ed25519.Verify(pub, message, sig) {
		return errBadSignature
	}
	return nil
}

// decodeSignature accepts base64 (the documented encoding) and hex (used by some
// examples) so a signature is never rejected on encoding alone.
func decodeSignature(signature string) ([]byte, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return nil, errBadSignature
	}
	if sig, err := base64.StdEncoding.DecodeString(signature); err == nil && len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	if sig, err := hex.DecodeString(signature); err == nil && len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	return nil, errBadSignature
}

// verifySignedRequest applies the fail-closed policy: a request carrying either signing
// header must verify; a request carrying neither is left to the adapter's capability
// check (the unguessable per-call path plus the call_control_id/connection_id match).
func (a *Adapter) verifySignedRequest(headers http.Header, payload []byte) error {
	signature := headers.Get(signatureHeader)
	timestamp := headers.Get(timestampHeader)
	if signature == "" && timestamp == "" {
		return nil
	}
	if signature == "" || timestamp == "" {
		return errBadSignature
	}
	if len(a.publicKey) == 0 {
		return errNoPublicKey
	}
	return verifySignature(a.publicKey, timestamp, signature, payload, time.Now())
}

// verifyStreamHandshake verifies the media-stream WebSocket handshake, whose signed
// payload is `{timestamp}|{stream_url}` for the exact URL configured in streaming_start.
func (a *Adapter) verifyStreamHandshake(headers http.Header, streamURL string) error {
	return a.verifySignedRequest(headers, []byte(streamURL))
}
