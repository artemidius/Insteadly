package mistral

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// maxSSEEventBytes bounds a single SSE event's accumulated data.
const maxSSEEventBytes = 1 << 20 // 1 MiB

// readSSEEvent reads one SSE event, returning its accumulated data payload (repeated
// data: lines joined by \n) and the event name (empty when absent). It accepts LF and
// CRLF, ignores `:` comments and unknown fields, preserves a trailing non-newline-terminated
// data line, and enforces the size cap while reading (never after an unbounded ReadBytes).
// io.EOF is returned at end of stream.
func readSSEEvent(br *bufio.Reader, maxBytes int) ([]byte, string, error) {
	var data []byte
	var event string
	sawData := false
	for {
		line, err := readSSELine(br, maxBytes)
		if errors.Is(err, io.EOF) {
			if sawData {
				return data, event, nil
			}
			return nil, "", io.EOF
		}
		if err != nil {
			return nil, "", err
		}
		if len(line) == 0 {
			if sawData {
				return data, event, nil
			}
			continue
		}
		if line[0] == ':' {
			continue // comment / keepalive
		}
		field, value := splitSSEField(line)
		switch field {
		case "data":
			if sawData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			sawData = true
			if len(data) > maxBytes {
				return nil, "", errors.New("mistral dialogue: sse event exceeds size limit")
			}
		case "event":
			event = string(value)
		}
	}
}

// readSSELine reads one line (trailing \n and \r stripped) with a hard cap so a newline-free
// line cannot allocate unbounded memory. A final unterminated line is returned as a line.
func readSSELine(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	sawAny := false
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			sawAny = true
		}
		line = append(line, frag...)
		if len(line) > maxBytes {
			return nil, errors.New("mistral dialogue: sse line exceeds size limit")
		}
		switch err {
		case nil:
			return trimLineEnding(line), nil
		case io.EOF:
			if !sawAny {
				return nil, io.EOF
			}
			return trimLineEnding(line), nil
		default:
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			return nil, err
		}
	}
}

func trimLineEnding(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	return b
}

func splitSSEField(line []byte) (string, []byte) {
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		v := line[i+1:]
		if len(v) > 0 && v[0] == ' ' {
			v = v[1:]
		}
		return string(line[:i]), v
	}
	return string(line), nil
}
