package domain

// Canonical audio everywhere inside the core: PCM16 little-endian, mono, 16 kHz.
// All codec/sample-rate conversion happens inside adapters (e.g. Twilio's 8 kHz µ-law
// is converted by the Twilio adapter). Providers must never leak formats past the
// port boundary.
const (
	SampleRate = 16000
	Channels   = 1
	BitDepth   = 16
	FrameMs    = 20
	FrameBytes = SampleRate * Channels * BitDepth / 8 * FrameMs / 1000 // = 640
)

// AudioChunk is a slice of canonical PCM16 LE mono 16 kHz audio.
type AudioChunk struct {
	Data []byte
}

// Blob keys for the storage port.
const (
	BlobKindRecording  = "recording"
	BlobKindTranscript = "transcript"
)

// BlobKey addresses one blob in the storage port. OrderID empty means a system-wide
// blob (not tied to an order); Kind must be one of the BlobKind* constants; Name is a
// plain filename with no path separators.
type BlobKey struct {
	OrderID OrderID // "" = system-wide blob (not tied to an order)
	Kind    string  // BlobKind*; storage validates against the allowlist
	Name    string  // filename, e.g. "2026-11-20T09-15-00Z.wav"
}
