package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func sendStreamChunk(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	chunk cliproxyexecutor.StreamChunk,
) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendStreamPayloads(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, payloads [][]byte) bool {
	for i := range payloads {
		if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: payloads[i]}) {
			return false
		}
	}
	return true
}

func scannerOrContextError(ctx context.Context, scanner *bufio.Scanner) error {
	if scanner != nil {
		if err := scanner.Err(); err != nil {
			return err
		}
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

func sseFrameLen(data []byte) int {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return 0
		}
		return crlf + 4
	case crlf < 0:
		return lf + 2
	case lf < crlf:
		return lf + 2
	default:
		return crlf + 4
	}
}

func scanSSEFrame(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if frameLen := sseFrameLen(data); frameLen > 0 {
		return frameLen, data[:frameLen], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func isCompleteSSEFrame(frame []byte) bool {
	return len(frame) > 0 && sseFrameLen(frame) == len(frame)
}

func sseDataPayload(frame []byte) ([]byte, bool, bool) {
	var dataLines [][]byte
	for len(frame) > 0 {
		line := frame
		if lineEnd := bytes.IndexByte(frame, '\n'); lineEnd >= 0 {
			line = frame[:lineEnd]
			frame = frame[lineEnd+1:]
		} else {
			frame = nil
		}
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		dataLines = append(dataLines, bytes.TrimSpace(line[len(dataTag):]))
	}
	if len(dataLines) == 0 {
		return nil, false, true
	}
	payload := bytes.Join(dataLines, []byte("\n"))
	if len(payload) == 0 {
		return payload, true, false
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return payload, true, true
	}
	return payload, true, json.Valid(payload)
}

func encodeSSEDataFrame(payload []byte) []byte {
	if json.Valid(payload) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, payload); err == nil {
			payload = compact.Bytes()
		}
	}
	out := make([]byte, 0, len(dataTag)+1+len(payload)+2)
	out = append(out, dataTag...)
	out = append(out, ' ')
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}
