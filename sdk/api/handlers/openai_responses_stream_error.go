package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIResponsesStreamErrorChunk struct {
	Type           string `json:"type"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	SequenceNumber int    `json:"sequence_number"`
}

type openAIResponsesStreamFailedChunk struct {
	Type           string                              `json:"type"`
	SequenceNumber int                                 `json:"sequence_number"`
	Response       openAIResponsesStreamFailedResponse `json:"response"`
}

type openAIResponsesStreamFailedResponse struct {
	Status string         `json:"status"`
	Error  map[string]any `json:"error"`
}

func openAIResponsesStreamErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusRequestTimeout:
		return "request_timeout"
	default:
		if status >= http.StatusInternalServerError {
			return "internal_server_error"
		}
		if status >= http.StatusBadRequest {
			return "invalid_request_error"
		}
		return "unknown_error"
	}
}

// extractErrorFields reads "message" and "code" from a map and returns updated values.
func extractErrorFields(fields map[string]any, message, code string) (string, string) {
	if msg, ok := fields["message"].(string); ok && strings.TrimSpace(msg) != "" {
		message = strings.TrimSpace(msg)
	}
	if v, ok := fields["code"]; ok && v != nil {
		if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
			code = strings.TrimSpace(c)
		} else {
			code = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return message, code
}

// BuildOpenAIResponsesStreamErrorChunk builds an OpenAI Responses streaming error chunk.
//
// Important: OpenAI's HTTP error bodies are shaped like {"error":{...}}; those are valid for
// non-streaming responses, but streaming clients validate SSE `data:` payloads against a union
// of chunks that requires a top-level `type` field.
func BuildOpenAIResponsesStreamErrorChunk(status int, errText string, sequenceNumber int) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if sequenceNumber < 0 {
		sequenceNumber = 0
	}

	message := strings.TrimSpace(errText)
	if message == "" {
		message = http.StatusText(status)
	}

	code := openAIResponsesStreamErrorCode(status)

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var payload map[string]any
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err == nil {
			if t, ok := payload["type"].(string); ok && strings.TrimSpace(t) == "error" {
				message, code = extractErrorFields(payload, message, code)
			}
			if value, ok := payload["sequence_number"].(json.Number); ok && sequenceNumber == 0 {
				if number, err := value.Int64(); err == nil && number >= 0 && int64(int(number)) == number {
					sequenceNumber = int(number)
				}
			}
			if e, ok := payload["error"].(map[string]any); ok {
				message, code = extractErrorFields(e, message, code)
			}
		}
	}

	if strings.TrimSpace(code) == "" {
		code = "unknown_error"
	}

	data, err := json.Marshal(
		openAIResponsesStreamErrorChunk{
			Type:           "error",
			Code:           code,
			Message:        message,
			SequenceNumber: sequenceNumber,
		},
	)
	if err == nil {
		return data
	}

	// Extremely defensive fallback.
	data, _ = json.Marshal(
		openAIResponsesStreamErrorChunk{
			Type:           "error",
			Code:           "internal_server_error",
			Message:        message,
			SequenceNumber: sequenceNumber,
		},
	)
	if len(data) > 0 {
		return data
	}
	return []byte(`{"type":"error","code":"internal_server_error","message":"internal error","sequence_number":0}`)
}

func openAIResponsesStreamFailedErrorDetail(status int, errText, code, message string) map[string]any {
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(errText)))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&payload); errDecode == nil && json.Valid([]byte(errText)) {
		if errorDetail, ok := payload["error"].(map[string]any); ok {
			return errorDetail
		}
		if response, ok := payload["response"].(map[string]any); ok {
			if errorDetail, ok := response["error"].(map[string]any); ok {
				return errorDetail
			}
		}
	}

	errorType := "invalid_request_error"
	if status == http.StatusRequestTimeout || status >= http.StatusInternalServerError {
		errorType = "server_error"
	}
	return map[string]any{
		"type":    errorType,
		"code":    code,
		"message": message,
	}
}

// BuildOpenAIResponsesStreamFailedChunk builds the terminal Responses event used by official Codex clients.
func BuildOpenAIResponsesStreamFailedChunk(status int, errText string, sequenceNumber int) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if sequenceNumber < 0 {
		sequenceNumber = 0
	}

	legacyChunk := BuildOpenAIResponsesStreamErrorChunk(status, errText, sequenceNumber)
	var legacyPayload openAIResponsesStreamErrorChunk
	if errUnmarshal := json.Unmarshal(legacyChunk, &legacyPayload); errUnmarshal != nil {
		legacyPayload.Code = openAIResponsesStreamErrorCode(status)
		legacyPayload.Message = http.StatusText(status)
		legacyPayload.SequenceNumber = sequenceNumber
	}
	if sequenceNumber == 0 && legacyPayload.SequenceNumber > 0 {
		sequenceNumber = legacyPayload.SequenceNumber
	}

	data, errMarshal := json.Marshal(
		openAIResponsesStreamFailedChunk{
			Type:           "response.failed",
			SequenceNumber: sequenceNumber,
			Response: openAIResponsesStreamFailedResponse{
				Status: "failed",
				Error: openAIResponsesStreamFailedErrorDetail(
					status,
					errText,
					legacyPayload.Code,
					legacyPayload.Message,
				),
			},
		},
	)
	if errMarshal == nil {
		return data
	}

	return []byte(`{"type":"response.failed","sequence_number":0,"response":{"status":"failed","error":{"type":"server_error","code":"internal_server_error","message":"internal error"}}}`)
}
