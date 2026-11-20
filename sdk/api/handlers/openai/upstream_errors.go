package openai

import (
	"bytes"
	"encoding/json"
	"net"
	"regexp"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

var (
	upstreamErrorSensitiveValue = regexp.MustCompile(
		`(?i)("?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|authorization|secret|password|passwd)` +
			`"?\s*[=:]\s*"?)([^\s"&,;}]+)`,
	)
	upstreamErrorBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	upstreamErrorURL    = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s"'<>]+`)
	upstreamErrorIP     = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\[[0-9a-fA-F:.]+]`)
)

func redactOpenAIUpstreamErrorText(text string) string {
	text = upstreamErrorBearer.ReplaceAllString(text, "Bearer [REDACTED]")
	text = upstreamErrorSensitiveValue.ReplaceAllString(text, `${1}[REDACTED]`)
	text = upstreamErrorURL.ReplaceAllString(text, "[REDACTED]")
	text = upstreamErrorIP.ReplaceAllStringFunc(
		text, func(value string) string {
			if address := net.ParseIP(strings.Trim(value, "[]")); address != nil &&
				(address.IsPrivate() ||
					address.IsLoopback() ||
					address.IsLinkLocalUnicast() ||
					address.IsLinkLocalMulticast() ||
					address.IsUnspecified()) {
				return "[REDACTED]"
			}
			return value
		},
	)
	characters := 0
	for index := range text {
		if characters == 2048 {
			return text[:index] + "…"
		}
		characters++
	}
	return text
}

func sanitizeOpenAIUpstreamErrorNode(value any) any {
	switch typed := value.(type) {
	case string:
		return redactOpenAIUpstreamErrorText(typed)
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized := strings.ReplaceAll(strings.ToLower(key), "-", "_")
			switch normalized {
			case "tokens", "token_count", "token_limit", "token_usage":
				cleaned[key] = sanitizeOpenAIUpstreamErrorNode(item)
			default:
				if normalized == "client_key" || util.IsSensitiveFieldName(key) {
					cleaned[key] = "[REDACTED]"
				} else {
					cleaned[key] = sanitizeOpenAIUpstreamErrorNode(item)
				}
			}
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(typed))
		for index, item := range typed {
			cleaned[index] = sanitizeOpenAIUpstreamErrorNode(item)
		}
		return cleaned
	default:
		return value
	}
}

func safeOpenAIUpstreamErrorText(text string) string {
	if !json.Valid([]byte(text)) {
		return redactOpenAIUpstreamErrorText(text)
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return redactOpenAIUpstreamErrorText(text)
	}
	if fields, ok := root.(map[string]any); ok {
		errorNode, found := fields["error"].(map[string]any)
		if !found {
			if response, ok := fields["response"].(map[string]any); ok {
				errorNode, found = response["error"].(map[string]any)
			}
		}
		if found {
			cleaned := map[string]any{"error": errorNode}
			if sequence, exists := fields["sequence_number"]; exists {
				cleaned["sequence_number"] = sequence
			}
			root = cleaned
		}
	}
	encoded, err := json.Marshal(sanitizeOpenAIUpstreamErrorNode(root))
	if err != nil {
		log.Warn("failed to encode sanitized upstream error")
		return "upstream request failed"
	}
	return string(encoded)
}

func sanitizeOpenAIUpstreamErrorFrame(frame []byte) ([]byte, error) {
	payload, hasData, _ := responsesSSEDataPayload(frame)
	if !hasData {
		return frame, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	cleaned, err := json.Marshal(sanitizeOpenAIUpstreamErrorNode(root))
	if err != nil {
		return nil, err
	}
	return replaceResponsesSSEData(frame, cleaned), nil
}

type openAIUpstreamClientError struct {
	message string
	cause   error
}

func (e *openAIUpstreamClientError) Error() string { return e.message }

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *openAIUpstreamClientError) Unwrap() error { return e.cause }

func sanitizeOpenAIUpstreamError(message *handlers.ErrorMessage) *handlers.ErrorMessage {
	if message == nil || message.DirectResponse || message.Error == nil {
		return message
	}
	cleaned := *message
	cleaned.Error = &openAIUpstreamClientError{
		message: safeOpenAIUpstreamErrorText(message.Error.Error()),
		cause:   message.Error,
	}
	return &cleaned
}
