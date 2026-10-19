package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responsesPatchTranslation struct {
	client   []byte
	original []byte
	bridge   *translatorcommon.ResponsesPatchBridge
	inner    any
	err      error
	closed   bool
}

type responseTranslationError struct {
	err error
}

func (e *responseTranslationError) Error() string   { return e.err.Error() }
func (e *responseTranslationError) Unwrap() error   { return e.err }
func (e *responseTranslationError) StatusCode() int { return http.StatusBadGateway }

// ResponseTranslationError 返回当前请求的转换错误，不改变必需的 SDK 接口
func ResponseTranslationError(param *any) error {
	if param != nil {
		if state, ok := (*param).(*responsesPatchTranslation); ok {
			return state.err
		}
		if state, ok := (*param).(interface{ ResponseTranslationError() error }); ok {
			if err := state.ResponseTranslationError(); err != nil {
				return &responseTranslationError{err: err}
			}
		}
	}
	return nil
}

func prepareResponsesPatchRequest(from, to Format, body []byte) ([]byte, error) {
	if from != FormatOpenAIResponse || to == FormatCodex || to == FormatOpenAIResponse ||
		translatorcommon.NewResponsesPatchBridge(body) == nil {
		return body, nil
	}
	return translatorcommon.NormalizeResponsesPatchTools(body)
}

func prepareResponsesPatchResponse(from, to Format, original []byte, param *any) (*responsesPatchTranslation, []byte, *any) {
	if to != FormatOpenAIResponse || from == FormatCodex || from == FormatOpenAIResponse {
		return nil, original, param
	}
	if param != nil {
		if state, ok := (*param).(*responsesPatchTranslation); ok {
			return state, state.original, &state.inner
		}
	}
	bridge := translatorcommon.NewResponsesPatchBridge(original)
	if bridge == nil {
		return nil, original, param
	}
	state := &responsesPatchTranslation{client: original, bridge: bridge}
	state.original, state.err = translatorcommon.NormalizeResponsesPatchTools(original)
	if from == FormatClaude || from == FormatInteractions {
		state.original = original
	}
	if state.err != nil {
		state.err = &responseTranslationError{err: state.err}
	}
	if param != nil {
		state.inner = *param
		*param = state
	}
	return state, state.original, &state.inner
}

func (s *responsesPatchTranslation) CanFinalizeResponseStream() bool {
	if s == nil || s.closed || s.err != nil {
		return false
	}
	state, ok := s.inner.(interface{ CanFinalizeResponseStream() bool })
	return ok && state.CanFinalizeResponseStream()
}

func (s *responsesPatchTranslation) validateStreamEnd(from Format, body []byte) {
	if s == nil || s.closed || s.err != nil || from != FormatOpenAI {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(body), []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) && !s.CanFinalizeResponseStream() {
		s.err = &responseTranslationError{err: fmt.Errorf("apply_patch upstream stream ended before finish_reason")}
	}
}

func (s *responsesPatchTranslation) fail(err error) []byte {
	if s.err == nil {
		s.err = &responseTranslationError{err: err}
	}
	s.bridge = nil
	s.inner = nil
	s.closed = true
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed",
			"error":  map[string]string{"code": "invalid_tool_arguments", "message": s.err.Error()},
		},
	})
	return payload
}

func responsesPatchFailure(kind string) bool {
	return kind == "response.failed" || kind == "response.error" || kind == "error"
}

func (s *responsesPatchTranslation) event(payload []byte) ([][]byte, error) {
	kind := gjson.GetBytes(payload, "type").String()
	if s.closed {
		if responsesPatchFailure(kind) {
			return [][]byte{payload}, nil
		}
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return nil, fmt.Errorf("apply_patch response ended before a JSON terminal event")
	}
	for _, field := range []string{"tools", "tool_choice"} {
		if value := gjson.GetBytes(s.client, field); value.Exists() && gjson.GetBytes(payload, "response."+field).Exists() {
			payload, _ = sjson.SetRawBytes(payload, "response."+field, []byte(value.Raw))
		}
	}
	events, err := s.bridge.Transform(payload)
	if err != nil {
		return nil, err
	}
	if kind == "response.done" {
		for i := range events {
			if gjson.GetBytes(events[i], "type").String() == kind {
				events[i], _ = sjson.SetBytes(events[i], "type", "response.completed")
			}
		}
	}
	if kind == "response.completed" || kind == "response.done" || kind == "response.incomplete" || responsesPatchFailure(kind) {
		s.closed = true
		s.bridge = nil
	}
	return events, nil
}

func (s *responsesPatchTranslation) release() {
	if s.closed {
		s.client = nil
		s.original = nil
		s.inner = nil
		s.bridge = nil
	}
}

func (s *responsesPatchTranslation) stream(frames [][]byte) [][]byte {
	if s == nil {
		return frames
	}
	defer s.release()
	if validator, ok := s.inner.(interface{ ValidateResponseToolIdentity([]byte) error }); ok && s.err == nil {
		if err := validator.ValidateResponseToolIdentity(s.client); err != nil {
			s.err = &responseTranslationError{err: err}
		}
	}
	if s.err == nil {
		s.err = ResponseTranslationError(&s.inner)
	}
	if s.err != nil {
		if s.closed {
			return nil
		}
		return [][]byte{responsesPatchSSE(s.fail(s.err))}
	}
	var out [][]byte
	for _, frame := range frames {
		trimmed := bytes.TrimSpace(frame)
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] == '{' || bytes.Equal(trimmed, []byte("[DONE]")) {
			events, err := s.event(trimmed)
			if err != nil {
				return [][]byte{responsesPatchSSE(s.fail(err))}
			}
			out = append(out, events...)
			continue
		}
		for _, raw := range bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
			var data [][]byte
			for _, line := range bytes.Split(raw, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) {
					data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
				}
			}
			if len(data) == 0 {
				continue
			}
			events, err := s.event(bytes.Join(data, []byte("\n")))
			if err != nil {
				return [][]byte{responsesPatchSSE(s.fail(err))}
			}
			for _, event := range events {
				out = append(out, responsesPatchSSE(event))
			}
		}
	}
	return out
}

func responsesPatchSSE(event []byte) []byte {
	kind := gjson.GetBytes(event, "type").String()
	frame := make([]byte, 0, len(event)+len(kind)+18)
	frame = append(frame, "event: "...)
	frame = append(frame, kind...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, event...)
	return append(frame, '\n', '\n')
}

func (s *responsesPatchTranslation) nonStream(body []byte) []byte {
	if s == nil {
		return body
	}
	defer s.release()
	if s.err == nil {
		s.err = ResponseTranslationError(&s.inner)
	}
	if s.err != nil {
		return s.fail(s.err)
	}
	if upstreamError := gjson.GetBytes(body, "error"); (upstreamError.Exists() && upstreamError.Type != gjson.Null) || gjson.GetBytes(body, "status").String() == "failed" {
		s.closed = true
		s.bridge = nil
		return body
	}
	kind := "response.completed"
	if gjson.GetBytes(body, "status").String() == "incomplete" {
		kind = "response.incomplete"
	}
	event, _ := sjson.SetBytes([]byte(`{}`), "type", kind)
	event, err := sjson.SetRawBytes(event, "response", body)
	if err != nil {
		return s.fail(err)
	}
	events, err := s.event(event)
	if err != nil {
		return s.fail(err)
	}
	for _, event := range events {
		if gjson.GetBytes(event, "type").String() == kind {
			return []byte(gjson.GetBytes(event, "response").Raw)
		}
	}
	return s.fail(fmt.Errorf("apply_patch response is missing its terminal payload"))
}
