package util

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMaskSensitiveHeaderValue(t *testing.T) {
	cases := []struct {
		key string
		raw string
	}{
		{key: "Authorization", raw: "Bearer secret-token"},
		{key: "X-Api-Key", raw: "sk-secret-value"},
		{key: "X-Management-Key", raw: "management-secret-value"},
		{key: "X-AuthToken", raw: "auth-token-secret-value"},
		{key: "X-Access-Key", raw: "access-key-secret-value"},
		{key: "Cookie", raw: "session=secret-cookie"},
		{key: "Set-Cookie", raw: "session=response-secret"},
	}

	for _, tc := range cases {
		t.Run(
			tc.key, func(t *testing.T) {
				got := MaskSensitiveHeaderValue(tc.key, tc.raw)
				if got == tc.raw || strings.Contains(got, "secret") {
					t.Fatalf("MaskSensitiveHeaderValue(%q, %q) = %q, want masked value", tc.key, tc.raw, got)
				}
			},
		)
	}
}

func TestMaskSensitivePayloadMasksSensitiveStringSlices(t *testing.T) {
	payload := []byte(`{"api-keys":["sk-secret-value"],"models":["gpt-5"]}`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-secret-value") {
		t.Fatalf("MaskSensitivePayload kept API key in string slice: %q", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("MaskSensitivePayload masked non-secret model value: %q", got)
	}
}

func TestMaskSensitivePayloadPreservesLargeJSONIntegers(t *testing.T) {
	payload := []byte(`{"api_key":"sk-secret-value","requests":9007199254740993}`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-secret-value") {
		t.Fatalf("MaskSensitivePayload kept API key: %q", got)
	}
	if !strings.Contains(got, `"requests": 9007199254740993`) {
		t.Fatalf("MaskSensitivePayload changed large integer: %q", got)
	}
}

func TestMaskSensitivePayloadDoesNotDiscardTrailingJSON(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("{\"api_key\":\"sk-secret-value\"}\n{\"model\":\"gpt-5\"}"),
		[]byte("{\"api_key\":\"sk-secret-value\"} {\"model\":\"gpt-5\"}"),
	} {
		got := string(MaskSensitivePayload(payload))
		if strings.Contains(got, "sk-secret-value") {
			t.Fatalf("MaskSensitivePayload kept API key: %q", got)
		}
		if !strings.Contains(got, `{"model":"gpt-5"}`) {
			t.Fatalf("MaskSensitivePayload discarded trailing JSON value: %q", got)
		}
	}
}

func TestMaskSensitivePayloadRedactsStructuredSecretsInJSONSequences(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("{\"api_key\":{\"nested\":\"sk-structured-secret\"}}\n{\"model\":\"gpt-5\"}"),
		[]byte("{\"api_key\":[\"sk-structured-secret\"]} {\"model\":\"gpt-5\"}"),
	} {
		got := string(MaskSensitivePayload(payload))
		if strings.Contains(got, "sk-structured-secret") || strings.Contains(got, "nested") {
			t.Fatalf("MaskSensitivePayload kept structured secret: %q", got)
		}
		if !strings.Contains(got, `{"model":"gpt-5"}`) {
			t.Fatalf("MaskSensitivePayload discarded non-sensitive JSON value: %q", got)
		}
	}
}

func TestMaskSensitivePayloadRedactsStructuredSecretsInSSEFrames(t *testing.T) {
	payload := []byte("data: {\"api_key\":{\"nested\":\"sk-structured-secret\"}}\n\ndata: {\"model\":\"gpt-5\"}\n\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-structured-secret") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept SSE structured secret: %q", got)
	}
	if !strings.Contains(got, `data: {"model":"gpt-5"}`) {
		t.Fatalf("MaskSensitivePayload discarded SSE frame: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsMultiLineSSEJSONEvents(t *testing.T) {
	payload := []byte("event: message\n" +
		"data: {\n" +
		"data: \"api_key\": {\"nested\": \"sk-multiline-secret\"},\n" +
		"data: \"model\": \"gpt-5\"\n" +
		"data: }\n\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-multiline-secret") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept multi-line SSE secret: %q", got)
	}
	if !strings.Contains(got, `"model":"gpt-5"`) {
		t.Fatalf("MaskSensitivePayload discarded multi-line SSE content: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsSensitiveObjectValues(t *testing.T) {
	payload := []byte(`{"api_key":{"nested":"sk-secret-value"},"model":"gpt-5"}`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-secret-value") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept sensitive object content: %q", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("MaskSensitivePayload masked non-sensitive content: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsAllSensitiveObjectSiblings(t *testing.T) {
	payload := []byte(`{"api_key":{"token":"decoy","nested":"sk-sibling-secret"}}`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-sibling-secret") || strings.Contains(got, "nested") || strings.Contains(got, "decoy") {
		t.Fatalf("MaskSensitivePayload kept sensitive object sibling: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsSensitiveFieldsInTextSSE(t *testing.T) {
	payload := []byte("data: api_key=sk-sse-text-secret\n\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-sse-text-secret") {
		t.Fatalf("MaskSensitivePayload kept text SSE secret: %q", got)
	}
	if !strings.Contains(got, "data: api_key=") {
		t.Fatalf("MaskSensitivePayload removed text SSE field: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsJSONAfterBodyTextPrefix(t *testing.T) {
	payload := []byte("Body:\nprefix\n{\"api_key\":{\"nested\":\"sk-prefixed-body-secret\"},\"model\":\"gpt-5\"}\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-prefixed-body-secret") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept prefixed Body secret: %q", got)
	}
	for _, value := range []string{"Body:", "prefix", "gpt-5"} {
		if !strings.Contains(got, value) {
			t.Fatalf("MaskSensitivePayload removed %q from prefixed Body: %q", value, got)
		}
	}
}

func TestMaskSensitivePayloadPreservesNumericTextPrefix(t *testing.T) {
	const payload = "200 OK from upstream"
	if got := string(MaskSensitivePayload([]byte(payload))); got != payload {
		t.Fatalf("MaskSensitivePayload numeric text = %q, want %q", got, payload)
	}
}

func TestMaskSensitivePayloadHandlesUTF8BOMBetweenJSONValues(t *testing.T) {
	payload := []byte("{\"api_key\":\"sk-first-bom-secret\"}\n\xEF\xBB\xBF{\"model\":\"gpt-5\"}")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-first-bom-secret") {
		t.Fatalf("MaskSensitivePayload kept BOM sequence secret: %q", got)
	}
	if !strings.Contains(got, `{"model":"gpt-5"}`) {
		t.Fatalf("MaskSensitivePayload discarded BOM sequence value: %q", got)
	}
}

func TestMaskSensitivePayloadFailsClosedAfterTruncatedJSONSequence(t *testing.T) {
	payload := []byte("{\"api_key\":{\"nested\":\"sk-sequence-secret\"}}\n{\"model\":")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-sequence-secret") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept secret before truncated JSON: %q", got)
	}
	if !strings.HasSuffix(got, "***") {
		t.Fatalf("MaskSensitivePayload did not redact truncated suffix: %q", got)
	}
}

func TestMaskSensitivePayloadFailsClosedForIncompleteJSON(t *testing.T) {
	payload := []byte("{\n  \"api_key\": {\n    \"nested\": \"sk-incomplete-secret\"")

	if got := string(MaskSensitivePayload(payload)); got != "***" {
		t.Fatalf("MaskSensitivePayload incomplete JSON = %q, want fail-closed marker", got)
	}
}

func TestMaskSensitivePayloadHandlesUTF8BOM(t *testing.T) {
	bom := []byte{0xef, 0xbb, 0xbf}
	jsonPayload := append(append([]byte{}, bom...), []byte(`{"api_key":"sk-bom-json-secret","model":"gpt-5"}`)...)
	maskedJSON := string(MaskSensitivePayload(jsonPayload))
	if strings.Contains(maskedJSON, "sk-bom-json-secret") || !strings.Contains(maskedJSON, "gpt-5") {
		t.Fatalf("MaskSensitivePayload BOM JSON mismatch: %q", maskedJSON)
	}

	ssePayload := append(append([]byte{}, bom...), []byte("data: {\"api_key\":\"sk-bom-sse-secret\",\"model\":\"gpt-5\"}\n\n")...)
	maskedSSE := string(MaskSensitivePayload(ssePayload))
	if strings.Contains(maskedSSE, "sk-bom-sse-secret") || !strings.Contains(maskedSSE, "gpt-5") {
		t.Fatalf("MaskSensitivePayload BOM SSE mismatch: %q", maskedSSE)
	}
}

func TestMaskSensitivePayloadFailsClosedForTruncatedTopLevelJSONTokens(t *testing.T) {
	for _, payload := range []string{`"sk-truncated-secret`, "tru", "fals", "nul", "-", "1e", "1."} {
		t.Run(payload, func(t *testing.T) {
			if got := string(MaskSensitivePayload([]byte(payload))); got != "***" {
				t.Fatalf("MaskSensitivePayload(%q) = %q, want fail-closed marker", payload, got)
			}
		})
	}
}

func TestMaskSensitivePayloadPreservesStructuredSecretContainers(t *testing.T) {
	payload := []byte(`{"gemini-api-key":[{"api-key":"sk-provider-secret","base-url":"https://example.com"}],"interactions-api-key":[{"api-key":"sk-interactions-secret","base-url":"https://interactions.example.com"}],"api-key-entries":[{"api-key":"sk-nested-secret","proxy-url":"http://127.0.0.1:8080"}]}`)

	got := string(MaskSensitivePayload(payload))
	for _, secret := range []string{"sk-provider-secret", "sk-interactions-secret", "sk-nested-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("MaskSensitivePayload kept structured secret %q: %s", secret, got)
		}
	}
	for _, value := range []string{"https://example.com", "https://interactions.example.com", "http://127.0.0.1:8080", "api-key"} {
		if !strings.Contains(got, value) {
			t.Fatalf("MaskSensitivePayload removed structured value %q: %s", value, got)
		}
	}
}

func TestMaskSensitivePayloadRedactsIndexedTextFields(t *testing.T) {
	payload := []byte("api_key[]: array-secret-value\napi_key[0]: indexed-secret-value\n")

	got := string(MaskSensitivePayload(payload))
	for _, secret := range []string{"array-secret-value", "indexed-secret-value"} {
		if strings.Contains(got, secret) {
			t.Fatalf("MaskSensitivePayload kept indexed text secret %q: %s", secret, got)
		}
	}
}

func TestMaskSensitivePayloadHandlesUTF8BOMBetweenSSEEvents(t *testing.T) {
	payload := []byte("data: {\"model\":\"gpt-5\"}\n\n\xEF\xBB\xBFdata: {\"api_key\":{\"nested\":\"sk-segment-bom-secret\"}}\n\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-segment-bom-secret") || strings.Contains(got, "nested") {
		t.Fatalf("MaskSensitivePayload kept BOM-delimited SSE secret: %q", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("MaskSensitivePayload removed non-sensitive SSE content: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsSensitiveMapKeys(t *testing.T) {
	rawA := "sk-alias-aone-zzzz"
	rawB := "sk-alias-btwo-zzzz"
	payload := []byte(`{"api-key-aliases":{"` + rawA + `":"a","` + rawB + `":"b"}}`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, rawA) || strings.Contains(got, rawB) {
		t.Fatalf("MaskSensitivePayload kept sensitive map key: %s", got)
	}
	if !strings.Contains(got, HideAPIKey(rawA)+"#1") || !strings.Contains(got, HideAPIKey(rawB)+"#2") {
		t.Fatalf("MaskSensitivePayload lost redacted map entries: %s", got)
	}
}

func TestMaskSensitivePayloadAddsOrdinalsToStructuredAPIKeyCollisions(t *testing.T) {
	rawA := "abcd-first-wxyz"
	rawB := "abcd-other-wxyz"
	masked := HideAPIKey(rawA)
	if masked != HideAPIKey(rawB) {
		t.Fatal("test API keys do not share a display mask")
	}
	payload := []byte(`{"gemini-api-key":[{"api-key":"` + rawA + `"},{"api-key":"` + rawB + `"}],"openai-compatibility":[{"api-key-entries":[{"api-key":"` + rawA + `"},{"api-key":"` + rawB + `"}]}]}`)

	var decoded struct {
		Gemini []struct {
			APIKey string `json:"api-key"`
		} `json:"gemini-api-key"`
		OpenAI []struct {
			APIKeyEntries []struct {
				APIKey string `json:"api-key"`
			} `json:"api-key-entries"`
		} `json:"openai-compatibility"`
	}
	maskedPayload := MaskSensitivePayload(payload)
	if err := json.Unmarshal(maskedPayload, &decoded); err != nil {
		t.Fatalf("decode MaskSensitivePayload result: %v payload=%s", err, maskedPayload)
	}
	want := []string{masked + "#1", masked + "#2"}
	if len(decoded.Gemini) != 2 || decoded.Gemini[0].APIKey != want[0] || decoded.Gemini[1].APIKey != want[1] {
		t.Fatalf("gemini API key masks = %#v, want %#v", decoded.Gemini, want)
	}
	if len(decoded.OpenAI) != 1 || len(decoded.OpenAI[0].APIKeyEntries) != 2 ||
		decoded.OpenAI[0].APIKeyEntries[0].APIKey != want[0] ||
		decoded.OpenAI[0].APIKeyEntries[1].APIKey != want[1] {
		t.Fatalf("OpenAI API key masks = %#v, want %#v", decoded.OpenAI, want)
	}
}

func TestMaskSensitivePayloadPreservesOpaqueSensitiveMapKeyValues(t *testing.T) {
	rawAPIKey := "sk-secret-value"
	payload := []byte(`{"apis":{"` + rawAPIKey + `":{"total_requests":1,"api_key":"` + rawAPIKey + `"}}}`)

	var decoded struct {
		APIs map[string]struct {
			TotalRequests int64  `json:"total_requests"`
			APIKey        string `json:"api_key"`
		} `json:"apis"`
	}
	masked := MaskSensitivePayload(payload)
	if err := json.Unmarshal(masked, &decoded); err != nil {
		t.Fatalf("decode MaskSensitivePayload result: %v payload=%s", err, masked)
	}
	entry, ok := decoded.APIs[HideAPIKey(rawAPIKey)]
	if !ok {
		t.Fatalf("MaskSensitivePayload lost API statistics entry: %s", masked)
	}
	if entry.TotalRequests != 1 {
		t.Fatalf("MaskSensitivePayload total requests = %d, want 1", entry.TotalRequests)
	}
	if entry.APIKey == rawAPIKey || entry.APIKey == "" {
		t.Fatalf("MaskSensitivePayload API key = %q, want non-empty masked value", entry.APIKey)
	}
}

func TestHideAPIKeyMasksShortValues(t *testing.T) {
	cases := map[string]string{
		"":         "***",
		"a":        "***",
		"abcd":     "***",
		"abcde":    "***",
		"abcdefgh": "***",
		"***":      "****",
	}

	for input, expected := range cases {
		t.Run(
			input, func(t *testing.T) {
				if got := HideAPIKey(input); got != expected {
					t.Fatalf("HideAPIKey(%q) = %q, want %q", input, got, expected)
				}
			},
		)
	}
}

func TestHideAPIKeyIsRuneSafeAndNeverSelfMasks(t *testing.T) {
	for _, input := range []string{"密钥甲乙丙丁戊己庚辛壬癸", "abcd...wxyz", "***"} {
		got := HideAPIKey(input)
		if !utf8.ValidString(got) {
			t.Fatalf("HideAPIKey(%q) returned invalid UTF-8 %q", input, got)
		}
		if got == input {
			t.Fatalf("HideAPIKey(%q) returned the original value", input)
		}
	}
}

func TestMaskSensitivePayload(t *testing.T) {
	jsonPayload := []byte(`{"model":"gpt-5.5","max_tokens":256,"maxTokens":128,"api_key":"sk-secret-value","apiKey":"camel-api-secret","managementKey":"management-secret","nested":{"access_token":"access-secret-value","accessToken":"camel-access-secret"}}`)
	maskedJSON := string(MaskSensitivePayload(jsonPayload))
	for _, secret := range []string{
		"sk-secret-value", "camel-api-secret", "management-secret", "access-secret-value", "camel-access-secret",
	} {
		if strings.Contains(maskedJSON, secret) {
			t.Fatalf("MaskSensitivePayload JSON kept secret %q: %s", secret, maskedJSON)
		}
	}
	if !strings.Contains(maskedJSON, "max_tokens") || !strings.Contains(
		maskedJSON, "256",
	) || !strings.Contains(maskedJSON, "maxTokens") || !strings.Contains(maskedJSON, "128") {
		t.Fatalf("MaskSensitivePayload JSON masked non-secret token limit fields: %s", maskedJSON)
	}

	textPayload := []byte("Authorization: Bearer auth-secret-value\nUpstream URL: https://example.test/v1?key=query-secret-value&model=gpt-5.5&accessToken=access-query-secret\nBody:\n{\"refreshToken\":\"refresh-secret-value\",\"maxTokens\":128}\n")
	maskedText := string(MaskSensitivePayload(textPayload))
	if strings.Contains(maskedText, "auth-secret-value") || strings.Contains(
		maskedText, "refresh-secret-value",
	) || strings.Contains(maskedText, "query-secret-value") || strings.Contains(maskedText, "access-query-secret") {
		t.Fatalf("MaskSensitivePayload text kept secret: %s", maskedText)
	}
	if !strings.Contains(maskedText, "maxTokens") || !strings.Contains(
		maskedText, "128",
	) || !strings.Contains(maskedText, "model=gpt-5.5") {
		t.Fatalf("MaskSensitivePayload text masked non-secret fields: %s", maskedText)
	}
}

func TestMaskSensitivePayloadRedactsEscapedJSONFieldNamesInText(t *testing.T) {
	payload := []byte(`prefix {"api` + "\x5c" + `u005fkey":"sk-escaped-field-secret"} suffix`)

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-escaped-field-secret") {
		t.Fatalf("MaskSensitivePayload kept escaped JSON field secret: %q", got)
	}
	if !strings.Contains(got, `api`+"\x5c"+`u005fkey`) {
		t.Fatalf("MaskSensitivePayload removed escaped JSON field name: %q", got)
	}
}

func TestMaskSensitivePayloadRedactsPreformattedBodySections(t *testing.T) {
	payload := []byte("=== API REQUEST 1 ===\n" +
		"Headers:\nContent-Type: application/json\n\n" +
		"Body:\n{\"api_key\":{\"nested\":\"sk-request-body-secret\"},\"model\":\"gpt-5\"}\n\n" +
		"=== API RESPONSE 1 ===\n" +
		"Body: {\"access_token\":{\"nested\":\"sk-response-body-secret\"},\"status\":\"ok\"}\n")

	got := string(MaskSensitivePayload(payload))
	for _, secret := range []string{"sk-request-body-secret", "sk-response-body-secret", "nested"} {
		if strings.Contains(got, secret) {
			t.Fatalf("MaskSensitivePayload kept preformatted body secret %q: %s", secret, got)
		}
	}
	for _, value := range []string{"=== API REQUEST 1 ===", "=== API RESPONSE 1 ===", "gpt-5", `"status": "ok"`} {
		if !strings.Contains(got, value) {
			t.Fatalf("MaskSensitivePayload removed preformatted value %q: %s", value, got)
		}
	}
}

func TestMaskSensitivePayloadPreservesWebsocketEventsAfterBody(t *testing.T) {
	payload := []byte("Timestamp: 2026-07-15T01:02:03Z\n" +
		"Event: api.websocket.request\n" +
		"Body:\n{\"api_key\":\"sk-websocket-secret\"}\n\n" +
		"Timestamp: 2026-07-15T01:02:04Z\n" +
		"Event: api.websocket.response\n" +
		"{\"type\":\"response.completed\",\"model\":\"gpt-5\"}\n")

	got := string(MaskSensitivePayload(payload))
	if strings.Contains(got, "sk-websocket-secret") {
		t.Fatalf("MaskSensitivePayload kept websocket body secret: %q", got)
	}
	for _, value := range []string{
		"Event: api.websocket.request",
		"Event: api.websocket.response",
		"response.completed",
		"gpt-5",
	} {
		if !strings.Contains(got, value) {
			t.Fatalf("MaskSensitivePayload removed websocket timeline value %q: %q", value, got)
		}
	}
}

func TestSanitizeFunctionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Normal", "valid_name", "valid_name"},
		{"With Dots", "name.with.dots", "name.with.dots"},
		{"With Colons", "name:with:colons", "name:with:colons"},
		{"With Dashes", "name-with-dashes", "name-with-dashes"},
		{"Mixed Allowed", "name.with_dots:colons-dashes", "name.with_dots:colons-dashes"},
		{"Invalid Characters", "name!with@invalid#chars", "name_with_invalid_chars"},
		{"Spaces", "name with spaces", "name_with_spaces"},
		{"Non-ASCII", "name_with_你好_chars", "name_with____chars"},
		{"Starts with digit", "123name", "_123name"},
		{"Starts with dot", ".name", "_.name"},
		{"Starts with colon", ":name", "_:name"},
		{"Starts with dash", "-name", "_-name"},
		{"Starts with invalid char", "!name", "_name"},
		{
			"Exactly 64 chars", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact",
			"this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact",
		},
		{
			"Too long (65 chars)", "this_is_a_very_long_name_that_exactly_reaches_sixty_four_charactX",
			"this_is_a_very_long_name_that_exactly_reaches_sixty_four_charact",
		},
		{
			"Very long", "this_is_a_very_long_name_that_exceeds_the_sixty_four_character_limit_for_function_names",
			"this_is_a_very_long_name_that_exceeds_the_sixty_four_character_l",
		},
		{
			"Starts with digit (64 chars total)", "1234567890123456789012345678901234567890123456789012345678901234",
			"_123456789012345678901234567890123456789012345678901234567890123",
		},
		{
			"Starts with invalid char (64 chars total)",
			"!234567890123456789012345678901234567890123456789012345678901234",
			"_234567890123456789012345678901234567890123456789012345678901234",
		},
		{"Empty", "", ""},
		{"Single character invalid", "@", "_"},
		{"Single character valid", "a", "a"},
		{"Single character digit", "1", "_1"},
		{"Single character underscore", "_", "_"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := sanitizeFunctionName(tt.input)
				if got != tt.expected {
					t.Errorf("sanitizeFunctionName(%q) = %v, want %v", tt.input, got, tt.expected)
				}
				// Verify Gemini compliance
				if len(got) > 64 {
					t.Errorf("sanitizeFunctionName(%q) result too long: %d", tt.input, len(got))
				}
				if len(got) > 0 {
					first := got[0]
					if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
						t.Errorf("sanitizeFunctionName(%q) result starts with invalid char: %c", tt.input, first)
					}
				}
			},
		)
	}
}
