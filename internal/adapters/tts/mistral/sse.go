package mistral

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// maxSSEEventBytes bounds a single SSE event's accumulated data.
const maxSSEEventBytes = 1 << 20 // 1 MiB

// readSSEEvent reads one SSE event's data payload (repeated data: lines joined by \n).
// It accepts LF and CRLF, ignores `:` comments and unknown fields, and returns io.EOF at
// end of stream. `[DONE]` is returned verbatim as data for the caller to interpret.
func readSSEEvent(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var data []byte
	sawData := false
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) > 0 && err == nil {
			if line[0] == ':' {
				continue
			}
			field, value := splitSSEField(line)
			if field == "data" {
				if sawData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				sawData = true
				if len(data) > maxBytes {
					return nil, errors.New("mistral: sse event exceeds size limit")
				}
			}
			continue
		}
		// len(line) == 0 (blank dispatch) or err != nil (EOF).
		if err != nil && err != io.EOF {
			return nil, err
		}
		if sawData {
			return data, nil
		}
		if err == io.EOF {
			return nil, io.EOF
		}
		// blank line with no data: keepalive/comment-only event; skip.
	}
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
