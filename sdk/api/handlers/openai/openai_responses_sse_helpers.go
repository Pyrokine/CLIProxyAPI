package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

func responsesSSEFrameLen(chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}
	lf := bytes.Index(chunk, []byte("\n\n"))
	crlf := bytes.Index(chunk, []byte("\r\n\r\n"))
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

func responsesSSEPrefixLenForPending(pending, chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}
	tailLen := min(len(pending), 3)
	headLen := min(len(chunk), 3)
	bridge := make([]byte, 0, tailLen+headLen)
	bridge = append(bridge, pending[len(pending)-tailLen:]...)
	bridge = append(bridge, chunk[:headLen]...)
	if frameLen := responsesSSEFrameLen(bridge); frameLen > tailLen {
		return frameLen - tailLen
	}
	return responsesSSEFrameLen(chunk)
}

func responsesSSEHasField(chunk []byte, prefix []byte) bool {
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func responsesSSEEventName(frame []byte) string {
	var event string
	for len(frame) > 0 {
		line := frame
		if index := bytes.IndexByte(frame, '\n'); index >= 0 {
			line = frame[:index]
			frame = frame[index+1:]
		} else {
			frame = nil
		}
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[len("event:"):]))
		}
	}
	return event
}

func shouldFilterPrivateResponsesSSEEvent(event string, isCodexClient bool) bool {
	event = strings.TrimSpace(event)
	if strings.HasPrefix(event, "responsesapi.") || event == "codex.rate_limits" {
		return true
	}
	return !isCodexClient && strings.HasPrefix(event, "codex.")
}

func responsesSSEDataPayload(chunk []byte) ([]byte, bool, bool) {
	var dataLines [][]byte
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
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
