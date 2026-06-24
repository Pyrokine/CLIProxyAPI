// Package util provides utility functions used across the CLIProxyAPI application.
// These functions handle common tasks such as determining AI service providers
// from model names and managing HTTP proxies.
package util

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

var (
	camelBoundaryPattern   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	jsonStringFieldPattern = regexp.MustCompile(`"([^"]+)"\s*:\s*"([^"\\]*(?:\\.[^"\\]*)*)"`)
)

// GetProviderName determines all AI service providers capable of serving a registered model.
// It first queries the global model registry to retrieve the providers backing the supplied model name.
// When the model has not been registered yet, it falls back to legacy string heuristics to infer
// potential providers.
//
// Supported providers include (but are not limited to):
//   - "gemini" for Google's Gemini family
//   - "codex" for OpenAI GPT-compatible providers
//   - "claude" for Anthropic models
//   - "qwen" for Alibaba's Qwen models
//   - "openai-compatibility" for external OpenAI-compatible providers
//
// Parameters:
//   - modelName: The name of the model to identify providers for.
//   - cfg: The application configuration containing OpenAI compatibility settings.
//
// Returns:
//   - []string: All provider identifiers capable of serving the model, ordered by preference.
func GetProviderName(modelName string) []string {
	if modelName == "" {
		return nil
	}

	providers := make([]string, 0, 4)
	seen := make(map[string]struct{})

	appendProvider := func(name string) {
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		providers = append(providers, name)
	}

	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(modelName) {
		appendProvider(provider)
	}

	if len(providers) > 0 {
		return providers
	}

	return providers
}

// ResolveAutoModel resolves the "auto" model name to an actual available model.
// It uses an empty handler type to get any available model from the registry.
//
// Parameters:
//   - modelName: The model name to check (should be "auto")
//
// Returns:
//   - string: The resolved model name, or the original if not "auto" or resolution fails
func ResolveAutoModel(modelName string) string {
	if modelName != "auto" {
		return modelName
	}

	// Use empty string as handler type to get any available model
	firstModel, err := registry.GetGlobalRegistry().GetFirstAvailableModel("")
	if err != nil {
		log.Warnf("Failed to resolve 'auto' model: %v, falling back to original model name", err)
		return modelName
	}

	log.Infof("Resolved 'auto' model to: %s", firstModel)
	return firstModel
}

// InArray checks if a string exists in a slice of strings.
// It iterates through the slice and returns true if the target string is found,
// otherwise it returns false.
//
// Parameters:
//   - hystack: The slice of strings to search in
//   - needle: The string to search for
//
// Returns:
//   - bool: True if the string is found, false otherwise
func InArray(hystack []string, needle string) bool {
	return slices.Contains(hystack, needle)
}

// HideAPIKey obscures an API key for logging purposes, showing only the first and last few characters.
//
// Parameters:
//   - apiKey: The API key to hide.
//
// Returns:
//   - string: The obscured API key.
func HideAPIKey(apiKey string) string {
	if len(apiKey) > 8 {
		return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
	} else if len(apiKey) > 4 {
		return apiKey[:2] + "..." + apiKey[len(apiKey)-2:]
	} else if len(apiKey) > 2 {
		return apiKey[:1] + "..." + apiKey[len(apiKey)-1:]
	}
	return apiKey
}

// maskAuthorizationHeader masks the Authorization header value while preserving the auth type prefix.
// Common formats: "Bearer <token>", "Basic <credentials>", "ApiKey <key>", etc.
// It preserves the prefix (e.g., "Bearer ") and only masks the token/credential part.
//
// Parameters:
//   - value: The Authorization header value
//
// Returns:
//   - string: The masked Authorization value with prefix preserved
func maskAuthorizationHeader(value string) string {
	parts := strings.SplitN(strings.TrimSpace(value), " ", 2)
	if len(parts) < 2 {
		return HideAPIKey(value)
	}
	return parts[0] + " " + HideAPIKey(parts[1])
}

// MaskSensitiveHeaderValue masks sensitive header values while preserving expected formats.
//
// Behavior by header key (case-insensitive):
//   - "Authorization": Preserve the auth type prefix (e.g., "Bearer ") and mask only the credential part.
//   - Headers containing "api-key", "token", "secret", or "cookie": Mask the entire value using HideAPIKey.
//   - Others: Return the original value unchanged.
//
// Parameters:
//   - key:   The HTTP header name to inspect (case-insensitive matching).
//   - value: The header value to mask when sensitive.
//
// Returns:
//   - string: The masked value according to the header type; unchanged if not sensitive.
func MaskSensitiveHeaderValue(key, value string) string {
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	switch {
	case strings.Contains(lowerKey, "authorization"):
		return maskAuthorizationHeader(value)
	case strings.Contains(lowerKey, "api-key"),
		strings.Contains(lowerKey, "apikey"),
		strings.Contains(lowerKey, "token"),
		strings.Contains(lowerKey, "secret"),
		strings.Contains(lowerKey, "cookie"):
		return HideAPIKey(value)
	default:
		return value
	}
}

// MaskSensitiveQuery masks sensitive query parameters, e.g. auth_token, within the raw query string.
func MaskSensitiveQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	changed := false
	for i, part := range parts {
		if part == "" {
			continue
		}
		keyPart := part
		valuePart := ""
		if before, after, ok := strings.Cut(part, "="); ok {
			keyPart = before
			valuePart = after
		}
		decodedKey, err := url.QueryUnescape(keyPart)
		if err != nil {
			decodedKey = keyPart
		}
		if !IsSensitiveFieldName(decodedKey) {
			continue
		}
		decodedValue, err := url.QueryUnescape(valuePart)
		if err != nil {
			decodedValue = valuePart
		}
		masked := HideAPIKey(strings.TrimSpace(decodedValue))
		parts[i] = keyPart + "=" + url.QueryEscape(masked)
		changed = true
	}
	if !changed {
		return raw
	}
	return strings.Join(parts, "&")
}

func IsSensitiveFieldName(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	key = strings.TrimSuffix(key, "[]")
	key = camelBoundaryPattern.ReplaceAllString(key, `${1}_${2}`)
	key = strings.ToLower(key)
	compact := strings.NewReplacer("-", "_", " ", "_").Replace(key)
	if compact == "key" || compact == "api_key" || compact == "apikey" || compact == "password" || compact == "passwd" || compact == "authorization" || compact == "management_key" || compact == "auth_key" || compact == "access_key" {
		return true
	}
	parts := strings.FieldsFunc(compact, func(r rune) bool { return r == '_' || r == '.' || r == ':' })
	for _, part := range parts {
		if part == "token" || part == "secret" || part == "cookie" || part == "credential" {
			return true
		}
	}
	return strings.Contains(compact, "api_key") || strings.Contains(compact, "apikey")
}

func MaskSensitivePayload(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return payload
	}
	var decoded any
	if json.Unmarshal(trimmed, &decoded) == nil {
		masked := maskSensitiveJSONValue(decoded, "")
		encoded, err := json.MarshalIndent(masked, "", "  ")
		if err == nil {
			return encoded
		}
	}
	return []byte(maskSensitiveText(string(payload)))
}

func maskSensitiveJSONValue(value any, key string) any {
	if IsSensitiveFieldName(key) {
		if text, ok := value.(string); ok {
			return HideAPIKey(text)
		}
		return "***"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			out[childKey] = maskSensitiveJSONValue(childValue, childKey)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, childValue := range typed {
			out[i] = maskSensitiveJSONValue(childValue, "")
		}
		return out
	default:
		return value
	}
}

func maskSensitiveText(payload string) string {
	maskedPayload := maskSensitiveQueryText(maskSensitiveEmbeddedJSONStrings(payload))
	changed := maskedPayload != payload
	lines := strings.Split(maskedPayload, "\n")
	for i, line := range lines {
		key, value, sep, ok := splitTextField(line)
		if !ok || !IsSensitiveFieldName(key) {
			continue
		}
		lines[i] = key + sep + " " + HideAPIKey(strings.TrimSpace(value))
		changed = true
	}
	if !changed {
		return payload
	}
	return strings.Join(lines, "\n")
}

func maskSensitiveQueryText(payload string) string {
	lines := strings.Split(payload, "\n")
	changed := false
	for i, line := range lines {
		question := strings.Index(line, "?")
		if question < 0 || question == len(line)-1 {
			continue
		}
		queryEnd := len(line)
		for j, r := range line[question+1:] {
			if r == ' ' || r == '\t' {
				queryEnd = question + 1 + j
				break
			}
		}
		maskedQuery := MaskSensitiveQuery(line[question+1 : queryEnd])
		if maskedQuery == line[question+1:queryEnd] {
			continue
		}
		lines[i] = line[:question+1] + maskedQuery + line[queryEnd:]
		changed = true
	}
	if !changed {
		return payload
	}
	return strings.Join(lines, "\n")
}

func maskSensitiveEmbeddedJSONStrings(payload string) string {
	matches := jsonStringFieldPattern.FindAllStringSubmatchIndex(payload, -1)
	if len(matches) == 0 {
		return payload
	}
	var builder strings.Builder
	cursor := 0
	changed := false
	for _, match := range matches {
		if len(match) < 6 || match[2] < 0 || match[3] < 0 || match[4] < 0 || match[5] < 0 {
			continue
		}
		key := payload[match[2]:match[3]]
		if !IsSensitiveFieldName(key) {
			continue
		}
		builder.WriteString(payload[cursor:match[4]])
		builder.WriteString(HideAPIKey(payload[match[4]:match[5]]))
		cursor = match[5]
		changed = true
	}
	if !changed {
		return payload
	}
	builder.WriteString(payload[cursor:])
	return builder.String()
}

func splitTextField(line string) (string, string, string, bool) {
	colon := strings.Index(line, ":")
	equal := strings.Index(line, "=")
	idx := -1
	sep := ""
	if colon >= 0 && (equal < 0 || colon < equal) {
		idx = colon
		sep = ":"
	} else if equal >= 0 {
		idx = equal
		sep = "="
	}
	if idx <= 0 {
		return "", "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	if key == "" || value == "" {
		return "", "", "", false
	}
	return key, value, sep, true
}
