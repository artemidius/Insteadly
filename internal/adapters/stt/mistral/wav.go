package mistral

import "encoding/binary"

// wavHeaderBytes is the size of a canonical PCM16 LE mono 16 kHz RIFF/WAVE header.
const wavHeaderBytes = 44

// wavHeader builds a 44-byte RIFF/WAVE header for headerless canonical PCM16 LE mono
// 16 kHz audio of pcmBytes length.
func wavHeader(pcmBytes int) []byte {
	h := make([]byte, wavHeaderBytes)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+pcmBytes))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)    // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:22], 1)     // audio format: integer PCM
	binary.LittleEndian.PutUint16(h[22:24], 1)     // channels: mono
	binary.LittleEndian.PutUint32(h[24:28], 16000) // sample rate
	binary.LittleEndian.PutUint32(h[28:32], 32000) // byte rate
	binary.LittleEndian.PutUint16(h[32:34], 2)     // block align
	binary.LittleEndian.PutUint16(h[34:36], 16)    // bits per sample
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(pcmBytes))
	return h
}
