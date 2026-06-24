package util

import (
	"strings"
	"testing"
)

func TestMaskSensitiveHeaderValue(t *testing.T) {
	cases := []struct {
		key string
		raw string
	}{
		{key: "Authorization", raw: "Bearer secret-token"},
		{key: "X-Api-Key", raw: "sk-secret-value"},
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
