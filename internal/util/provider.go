// Package util provides utility functions used across the CLIProxyAPI application.
// These functions handle common tasks such as determining AI service providers
// from model names and managing HTTP proxies.
package util

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

var (
	camelBoundaryPattern        = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	jsonStringFieldPattern      = regexp.MustCompile(`"([^"]+)"\s*:\s*"([^"\\]*(?:\\.[^"\\]*)*)"`)
	trailingFieldIndexesPattern = regexp.MustCompile(`(\[[0-9]*])+$`)
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
//   - haystack: The slice of strings to search in
//   - needle: The string to search for
//
// Returns:
//   - bool: True if the string is found, false otherwise
func InArray(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// HideAPIKey obscures an API key for logging purposes, showing only the first and last few characters.
//
// Parameters:
//   - apiKey: The API key to hide.
//
// Returns:
//   - string: The obscured API key.
func HideAPIKey(apiKey string) string {
	runes := []rune(apiKey)
	if len(runes) <= 8 {
		if apiKey == "***" {
			return "****"
		}
		return "***"
	}
	masked := string(runes[:4]) + "..." + string(runes[len(runes)-4:])
	if masked == apiKey {
		return string(runes[:4]) + "...." + string(runes[len(runes)-4:])
	}
	return masked
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
//   - Sensitive credential headers, including API, management, auth, access, token, secret, and cookie keys:
//     Mask the entire value using HideAPIKey.
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
	case IsSensitiveFieldName(lowerKey),
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
	key = trailingFieldIndexesPattern.ReplaceAllString(key, "")
	key = camelBoundaryPattern.ReplaceAllString(key, `${1}_${2}`)
	key = strings.ToLower(key)
	compact := strings.NewReplacer("-", "_", " ", "_").Replace(key)
	if compact == "key" || compact == "api_key" || compact == "apikey" || compact == "password" || compact == "passwd" || compact == "authorization" || compact == "management_key" || compact == "auth_key" || compact == "access_key" ||
		strings.HasSuffix(compact, "_management_key") || strings.HasSuffix(
		compact, "_auth_key",
	) || strings.HasSuffix(compact, "_access_key") {
		return true
	}
	parts := strings.FieldsFunc(compact, func(r rune) bool { return r == '_' || r == '.' || r == ':' })
	for _, part := range parts {
		if part == "token" || part == "secret" || part == "cookie" || part == "credential" {
			return true
		}
	}
	return strings.Contains(compact, "api_key") || strings.Contains(compact, "apikey") || strings.Contains(
		compact, "management_key",
	)
}

func MaskSensitivePayload(payload []byte) []byte {
	return maskSensitivePayload(payload, true)
}

func maskSensitivePayload(payload []byte, redactBodySections bool) []byte {
	payload = stripUTF8BOM(payload)
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return payload
	}
	if masked, ok := maskSingleJSON(trimmed, true); ok {
		return masked
	}
	if masked, ok := maskJSONSequence(payload); ok {
		return masked
	}
	if looksLikeJSON(trimmed) {
		return []byte("***")
	}
	return []byte(maskSensitiveText(string(payload), redactBodySections))
}

func stripUTF8BOM(payload []byte) []byte {
	bomOffset := 0
	for bomOffset < len(payload) && isJSONWhitespace(payload[bomOffset]) {
		bomOffset++
	}
	bom := []byte{0xef, 0xbb, 0xbf}
	if !bytes.HasPrefix(payload[bomOffset:], bom) {
		return payload
	}
	out := make([]byte, 0, len(payload)-len(bom))
	out = append(out, payload[:bomOffset]...)
	out = append(out, payload[bomOffset+len(bom):]...)
	return out
}

func maskSingleJSON(payload []byte, indent bool) ([]byte, bool) {
	payload = stripUTF8BOM(payload)
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	masked := maskSensitiveJSONValue(decoded, "")
	var (
		encoded []byte
		err     error
	)
	if indent {
		encoded, err = json.MarshalIndent(masked, "", "  ")
	} else {
		encoded, err = json.Marshal(masked)
	}
	return encoded, err == nil
}

func maskJSONSequence(payload []byte) ([]byte, bool) {
	var out bytes.Buffer
	cursor := 0
	values := 0
	bom := []byte{0xef, 0xbb, 0xbf}
	for {
		valueStart := cursor
		separator := make([]byte, 0, 4)
		for valueStart < len(payload) {
			switch {
			case isJSONWhitespace(payload[valueStart]):
				separator = append(separator, payload[valueStart])
				valueStart++
			case bytes.HasPrefix(payload[valueStart:], bom):
				valueStart += len(bom)
			default:
				goto separatorDone
			}
		}
	separatorDone:
		if valueStart == len(payload) {
			if values < 2 {
				return nil, false
			}
			out.Write(separator)
			return out.Bytes(), true
		}

		decoder := json.NewDecoder(bytes.NewReader(payload[valueStart:]))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			if values == 0 || !looksLikeJSON(payload[valueStart:]) {
				return nil, false
			}
			out.Write(separator)
			out.WriteString("***")
			return out.Bytes(), true
		}
		valueEnd := valueStart + int(decoder.InputOffset())
		if valueEnd <= valueStart || valueEnd > len(payload) {
			return nil, false
		}
		encoded, errMarshal := json.Marshal(maskSensitiveJSONValue(decoded, ""))
		if errMarshal != nil {
			return nil, false
		}
		out.Write(separator)
		out.Write(encoded)
		cursor = valueEnd
		values++
	}
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func looksLikeJSON(payload []byte) bool {
	trimmed := bytes.TrimSpace(stripUTF8BOM(payload))
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{', '[', '"':
		return true
	case 't':
		return looksLikeJSONLiteral(trimmed, "true")
	case 'f':
		return looksLikeJSONLiteral(trimmed, "false")
	case 'n':
		return looksLikeJSONLiteral(trimmed, "null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return looksLikeJSONNumber(trimmed)
	default:
		return false
	}
}

func looksLikeJSONLiteral(payload []byte, literal string) bool {
	wordEnd := 0
	for wordEnd < len(payload) {
		value := payload[wordEnd]
		if isJSONWhitespace(value) || value == ',' || value == ']' || value == '}' {
			break
		}
		wordEnd++
	}
	candidate := string(payload[:wordEnd])
	return strings.HasPrefix(literal, candidate) || strings.HasPrefix(candidate, literal)
}

func looksLikeJSONNumber(payload []byte) bool {
	for i, value := range payload {
		switch {
		case value >= '0' && value <= '9':
		case value == '-', value == '+', value == '.', value == 'e', value == 'E':
		case isJSONWhitespace(value):
			return len(bytes.TrimSpace(payload[i:])) == 0
		case value == ',', value == ']', value == '}':
			return true
		default:
			return false
		}
	}
	return true
}

type structuredAPIKeyMaskOrdinals map[string]map[string]string

func maskSensitiveJSONValue(value any, key string) any {
	return maskSensitiveJSONValueWithOrdinals(value, key, nil)
}

func maskSensitiveJSONValueWithOrdinals(
	value any,
	key string,
	ordinals structuredAPIKeyMaskOrdinals,
) any {
	if IsSensitiveFieldName(key) {
		switch typed := value.(type) {
		case map[string]any:
			if isStructuredSensitiveJSONContainer(key) {
				return maskSensitiveJSONMapWithOrdinals(typed, key, ordinals)
			}
			return "***"
		case []any:
			out := make([]any, len(typed))
			preserveStructuredValues := isStructuredSensitiveJSONContainer(key)
			if preserveStructuredValues && ordinals == nil {
				ordinals = newStructuredAPIKeyMaskOrdinals(typed)
			}
			for i, childValue := range typed {
				switch child := childValue.(type) {
				case map[string]any:
					if preserveStructuredValues {
						out[i] = maskSensitiveJSONMapWithOrdinals(child, key, ordinals)
					} else {
						out[i] = "***"
					}
				case []any:
					if preserveStructuredValues {
						out[i] = maskSensitiveJSONValueWithOrdinals(child, key, ordinals)
					} else {
						out[i] = "***"
					}
				default:
					out[i] = maskSensitiveJSONValueWithOrdinals(childValue, key, ordinals)
				}
			}
			return out
		case string:
			return maskSensitiveJSONString(typed, key, ordinals)
		default:
			return "***"
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		return maskSensitiveJSONMapWithOrdinals(typed, key, ordinals)
	case []any:
		out := make([]any, len(typed))
		for i, childValue := range typed {
			out[i] = maskSensitiveJSONValueWithOrdinals(childValue, "", ordinals)
		}
		return out
	case string:
		return maskSensitiveText(typed, true)
	default:
		return value
	}
}

func maskSensitiveJSONString(
	value string,
	key string,
	ordinals structuredAPIKeyMaskOrdinals,
) string {
	if strings.Contains(strings.ToLower(strings.TrimSpace(key)), "authorization") {
		return maskAuthorizationHeader(value)
	}
	masked := HideAPIKey(value)
	if !isStructuredAPIKeyJSONField(key) || ordinals == nil {
		return masked
	}
	if placeholder := ordinals[masked][value]; placeholder != "" {
		return placeholder
	}
	return masked
}

func newStructuredAPIKeyMaskOrdinals(value any) structuredAPIKeyMaskOrdinals {
	groups := make(map[string][]string)
	seen := make(map[string]map[string]struct{})
	collectStructuredAPIKeyValues(value, "", groups, seen)

	naturalMasks := make(map[string]struct{}, len(groups))
	for masked := range groups {
		naturalMasks[masked] = struct{}{}
	}
	ordinals := make(structuredAPIKeyMaskOrdinals)
	for masked, values := range groups {
		if len(values) < 2 {
			continue
		}
		ordinals[masked] = make(map[string]string, len(values))
		for i, raw := range values {
			placeholder := masked + "#" + strconv.Itoa(i+1)
			for {
				if _, collides := naturalMasks[placeholder]; !collides {
					break
				}
				placeholder += "#"
			}
			ordinals[masked][raw] = placeholder
		}
	}
	return ordinals
}

func collectStructuredAPIKeyValues(
	value any,
	key string,
	groups map[string][]string,
	seen map[string]map[string]struct{},
) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for childKey := range typed {
			keys = append(keys, childKey)
		}
		sort.Strings(keys)
		for _, childKey := range keys {
			collectStructuredAPIKeyValues(typed[childKey], childKey, groups, seen)
		}
	case []any:
		for _, childValue := range typed {
			collectStructuredAPIKeyValues(childValue, key, groups, seen)
		}
	case string:
		if !isStructuredAPIKeyJSONField(key) {
			return
		}
		masked := HideAPIKey(typed)
		if seen[masked] == nil {
			seen[masked] = make(map[string]struct{})
		}
		if _, ok := seen[masked][typed]; ok {
			return
		}
		seen[masked][typed] = struct{}{}
		groups[masked] = append(groups[masked], typed)
	}
}

func isStructuredAPIKeyJSONField(key string) bool {
	normalizedKey := camelBoundaryPattern.ReplaceAllString(strings.TrimSpace(key), `${1}_${2}`)
	normalizedKey = strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(normalizedKey))
	return normalizedKey == "api_key" || normalizedKey == "apikey"
}

func maskSensitiveJSONMapWithOrdinals(
	value map[string]any,
	parentKey string,
	ordinals structuredAPIKeyMaskOrdinals,
) map[string]any {
	out := make(map[string]any, len(value))
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	redactedCounts := make(map[string]int)
	redactedTotals := make(map[string]int)
	redactMapKeys := isSensitiveJSONMapKeyContainer(parentKey)
	if redactMapKeys {
		for _, childKey := range keys {
			trimmed := strings.TrimSpace(childKey)
			if trimmed == "" || trimmed == "(none)" {
				continue
			}
			redactedTotals[HideAPIKey(trimmed)]++
		}
	}
	for _, childKey := range keys {
		outKey := childKey
		valueKey := childKey
		if redactMapKeys {
			outKey = maskedJSONMapKey(redactedCounts, redactedTotals, childKey)
			valueKey = ""
		}
		out[outKey] = maskSensitiveJSONValueWithOrdinals(value[childKey], valueKey, ordinals)
	}
	return out
}

func maskedJSONMapKey(counts map[string]int, totals map[string]int, key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || trimmed == "(none)" {
		return key
	}
	base := HideAPIKey(trimmed)
	if totals[base] < 2 {
		return base
	}
	counts[base]++
	return base + "#" + strconv.Itoa(counts[base])
}

func isStructuredSensitiveJSONContainer(key string) bool {
	normalizedKey := camelBoundaryPattern.ReplaceAllString(strings.TrimSpace(key), `${1}_${2}`)
	normalizedKey = strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(normalizedKey))
	switch normalizedKey {
	case "gemini_api_key", "interactions_api_key", "claude_api_key", "codex_api_key", "xai_api_key", "vertex_api_key", "api_key_entries":
		return true
	default:
		return isSensitiveJSONMapKeyContainer(key)
	}
}

func isSensitiveJSONMapKeyContainer(key string) bool {
	normalizedKey := camelBoundaryPattern.ReplaceAllString(strings.TrimSpace(key), `${1}_${2}`)
	normalizedKey = strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(normalizedKey))
	switch normalizedKey {
	case "api_key_aliases", "api_key_rules", "apis":
		return true
	default:
		return false
	}
}

func maskSensitiveText(payload string, redactBodySections bool) string {
	maskedPayload := payload
	if redactBodySections {
		maskedPayload = maskSensitiveBodySections(maskedPayload)
	}
	maskedPayload = maskSensitiveJSONFrames(maskedPayload)
	maskedPayload = maskSensitiveQueryText(maskSensitiveEmbeddedJSONStrings(maskedPayload))
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

func maskSensitiveBodySections(payload string) string {
	lines := strings.Split(payload, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for index := 0; index < len(lines); {
		line := lines[index]
		bodyPrefix, inlineBody, ok := splitBodySectionLine(line)
		if !ok {
			out = append(out, line)
			index++
			continue
		}

		index++
		bodyEnd := index
		for bodyEnd < len(lines) && !isBodySectionBoundary(lines, bodyEnd) {
			bodyEnd++
		}
		contentEnd := bodyEnd
		for contentEnd > index && strings.TrimSpace(lines[contentEnd-1]) == "" {
			contentEnd--
		}

		bodyLines := make([]string, 0, contentEnd-index+1)
		if inlineBody != "" {
			bodyLines = append(bodyLines, inlineBody)
		}
		bodyLines = append(bodyLines, lines[index:contentEnd]...)
		if len(bodyLines) == 0 {
			out = append(out, line)
		} else {
			body := strings.Join(bodyLines, "\n")
			embeddedMasked := maskEmbeddedJSONValues(body)
			masked := string(maskSensitivePayload([]byte(embeddedMasked), false))
			maskedLines := strings.Split(masked, "\n")
			if inlineBody == "" {
				out = append(out, line)
				out = append(out, maskedLines...)
			} else {
				out = append(out, bodyPrefix+maskedLines[0])
				out = append(out, maskedLines[1:]...)
			}
			changed = changed || masked != body
		}
		out = append(out, lines[contentEnd:bodyEnd]...)
		index = bodyEnd
	}
	if !changed {
		return payload
	}
	return strings.Join(out, "\n")
}

func maskEmbeddedJSONValues(payload string) string {
	input := []byte(payload)
	var out bytes.Buffer
	cursor := 0
	changed := false
	for index := 0; index < len(input); index++ {
		if input[index] != '{' && input[index] != '[' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(input[index:]))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			continue
		}
		switch decoded.(type) {
		case map[string]any, []any:
		default:
			continue
		}
		valueEnd := index + int(decoder.InputOffset())
		if valueEnd <= index || valueEnd > len(input) {
			continue
		}
		originalEncoded, errOriginal := json.Marshal(decoded)
		maskedEncoded, errMasked := json.Marshal(maskSensitiveJSONValue(decoded, ""))
		if errOriginal != nil || errMasked != nil || bytes.Equal(originalEncoded, maskedEncoded) {
			continue
		}
		out.Write(input[cursor:index])
		out.Write(maskedEncoded)
		cursor = valueEnd
		index = valueEnd - 1
		changed = true
	}
	if !changed {
		return payload
	}
	out.Write(input[cursor:])
	return out.String()
}

func splitBodySectionLine(line string) (string, string, bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), "Body") {
		return "", "", false
	}
	bodyStart := colon + 1
	for bodyStart < len(line) && (line[bodyStart] == ' ' || line[bodyStart] == '\t') {
		bodyStart++
	}
	return line[:bodyStart], line[bodyStart:], true
}

func isAPISectionHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "=== ") && strings.HasSuffix(trimmed, " ===")
}

func isBodySectionBoundary(lines []string, index int) bool {
	if index < 0 || index >= len(lines) {
		return false
	}
	if isAPISectionHeader(lines[index]) {
		return true
	}
	if index+1 >= len(lines) {
		return false
	}
	timestampKey, _, _, timestampOK := splitTextField(lines[index])
	eventKey, _, _, eventOK := splitTextField(lines[index+1])
	return timestampOK && eventOK &&
		strings.EqualFold(strings.TrimSpace(timestampKey), "Timestamp") &&
		strings.EqualFold(strings.TrimSpace(eventKey), "Event")
}

func maskSensitiveJSONFrames(payload string) string {
	lines := strings.Split(payload, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for start := 0; start < len(lines); {
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		maskedEvent, eventChanged := maskSensitiveSSEEvent(lines[start:end])
		out = append(out, maskedEvent...)
		changed = changed || eventChanged
		if end < len(lines) {
			out = append(out, lines[end])
			end++
		}
		start = end
	}
	if !changed {
		return payload
	}
	return strings.Join(out, "\n")
}

func maskSensitiveSSEEvent(lines []string) ([]string, bool) {
	dataLineIndexes := make([]int, 0, len(lines))
	contentStarts := make([]int, 0, len(lines))
	contents := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		dataOffset := len(line) - len(trimmed)
		if after, ok := strings.CutPrefix(trimmed, "\xEF\xBB\xBF"); ok {
			trimmed = after
			dataOffset += len("\xEF\xBB\xBF")
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		contentStart := dataOffset + len("data:")
		for contentStart < len(line) && (line[contentStart] == ' ' || line[contentStart] == '\t') {
			contentStart++
		}
		dataLineIndexes = append(dataLineIndexes, i)
		contentStarts = append(contentStarts, contentStart)
		contents = append(contents, line[contentStart:])
	}
	if len(contents) == 0 {
		return lines, false
	}

	content := strings.Join(contents, "\n")
	if content == "" || content == "[DONE]" {
		return lines, false
	}
	masked, ok := maskSingleJSON([]byte(content), false)
	if !ok {
		if !looksLikeJSON([]byte(content)) {
			maskedText := maskSensitiveText(content, false)
			if maskedText == content {
				return lines, false
			}
			masked = []byte(maskedText)
		} else {
			masked = []byte("***")
		}
	}

	firstDataLine := dataLineIndexes[0]
	firstContentStart := contentStarts[0]
	out := make([]string, 0, len(lines)-len(dataLineIndexes)+1)
	nextDataLine := 1
	for i, line := range lines {
		switch {
		case i == firstDataLine:
			out = append(out, line[:firstContentStart]+string(masked))
		case nextDataLine < len(dataLineIndexes) && i == dataLineIndexes[nextDataLine]:
			nextDataLine++
		default:
			out = append(out, line)
		}
	}
	return out, true
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
		decodedKey := key
		if strings.Contains(key, `\`) {
			if err := json.Unmarshal([]byte(`"`+key+`"`), &decodedKey); err != nil {
				continue
			}
		}
		if !IsSensitiveFieldName(decodedKey) {
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
	if !isPlainTextFieldKey(key) || value == "" {
		return "", "", "", false
	}
	return key, value, sep, true
}

func isPlainTextFieldKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case ' ', '-', '_', '.', '[', ']':
			continue
		default:
			return false
		}
	}
	return true
}
