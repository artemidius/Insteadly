package recorder

import (
	"encoding/binary"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// wavHeaderSize is the canonical 44-byte RIFF/WAVE header for PCM16 with no extra
// chunks. It matches stt/mistral/wav.go, re-implemented here because the core must not
// import an adapter.
const wavHeaderSize = 44

// wavBytes wraps canonical PCM16 LE mono audio in a RIFF/WAVE container. The core's
// canonical format is fixed (domain.SampleRate / domain.Channels / domain.BitDepth), so
// the header never needs a caller-supplied format.
func wavBytes(pcm []byte) []byte {
	if len(pcm)%2 != 0 {
		// A WAV data chunk of odd length is legal but our canonical audio is whole
		// samples; dropping one trailing byte is both correct and simpler.
		pcm = pcm[:len(pcm)-1]
	}
	out := make([]byte, wavHeaderSize+len(pcm))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)                                                          // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)                                                           // PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(domain.Channels))                                     // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(domain.SampleRate))                                   // 16 kHz
	binary.LittleEndian.PutUint32(out[28:32], uint32(domain.SampleRate*domain.Channels*domain.BitDepth/8)) // byte rate
	binary.LittleEndian.PutUint16(out[32:34], uint16(domain.Channels*domain.BitDepth/8))                   // block align
	binary.LittleEndian.PutUint16(out[34:36], uint16(domain.BitDepth))                                     // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[wavHeaderSize:], pcm)
	return out
}
