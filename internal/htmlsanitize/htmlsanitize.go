package htmlsanitize

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"mime"
	"strings"
)

// escapeString escapes text before it is returned to browser-facing management clients.
func escapeString(value string) string {
	return html.EscapeString(value)
}

// jsonBody escapes all string values in a JSON document.
func jsonBody(body []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body, false
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return body, false
	}
	var extra any
	if errExtra := decoder.Decode(&extra); errExtra != io.EOF {
		return body, false
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(jsonValue(value)); errEncode != nil {
		return body, false
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), true
}

// jsonBodyIfLikely escapes JSON bodies when the content type or body shape indicates JSON.
func jsonBodyIfLikely(body []byte, contentType string) ([]byte, bool) {
	if isJSONContentType(contentType) || looksLikeJSON(body) {
		return jsonBody(body)
	}
	return body, false
}

// jsonValue recursively escapes string values in JSON-compatible data.
func jsonValue(value any) any {
	switch typed := value.(type) {
	case string:
		return escapeString(typed)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = jsonValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = jsonValue(item)
		}
		return out
	default:
		return value
	}
}

// isJSONContentType reports whether contentType is application/json or a +json type.
func isJSONContentType(contentType string) bool {
	mediaType, _, errParse := mime.ParseMediaType(strings.TrimSpace(contentType))
	if errParse != nil {
		mediaType = strings.TrimSpace(contentType)
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// looksLikeJSON reports whether body starts with an object or array JSON marker.
func looksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	return trimmed[0] == '{' || trimmed[0] == '['
}
