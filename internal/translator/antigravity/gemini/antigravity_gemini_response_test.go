package gemini

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

func TestRestoreUsageMetadata(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{
			name:     "cpaUsageMetadata renamed to usageMetadata",
			input:    []byte(`{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100,"candidatesTokenCount":200}}`),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":200}}`,
		},
		{
			name:     "no cpaUsageMetadata unchanged",
			input:    []byte(`{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
		{
			name:     "empty input",
			input:    []byte(`{}`),
			expected: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := restoreUsageMetadata(tt.input)
				if string(result) != tt.expected {
					t.Errorf("restoreUsageMetadata() = %s, want %s", string(result), tt.expected)
				}
			},
		)
	}
}

func TestConvertAntigravityResponseToGeminiNonStream(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{
			name:     "cpaUsageMetadata restored in response",
			input:    []byte(`{"response":{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100}}}`),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
		{
			name:     "usageMetadata preserved",
			input:    []byte(`{"response":{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}}`),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := convertAntigravityResponseToGeminiNonStream(context.Background(), "", nil, nil, tt.input, nil)
				if string(result) != tt.expected {
					t.Errorf("ConvertAntigravityResponseToGeminiNonStream() = %s, want %s", string(result), tt.expected)
				}
			},
		)
	}
}

func TestConvertAntigravityResponseToGeminiStream(t *testing.T) {
	ctx := util.WithAlt(context.Background(), "")

	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{
			name:     "cpaUsageMetadata restored in streaming response",
			input:    []byte(`data: {"response":{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100}}}`),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				results := convertAntigravityResponseToGemini(ctx, "", nil, nil, tt.input, nil)
				if len(results) != 1 {
					t.Fatalf("expected 1 result, got %d", len(results))
				}
				if string(results[0]) != tt.expected {
					t.Errorf("ConvertAntigravityResponseToGemini() = %s, want %s", string(results[0]), tt.expected)
				}
			},
		)
	}
}

func TestConvertAntigravityResponseToGeminiStreamRestoresDisambiguatedNames(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[{"functionDeclarations":[
		{"name":"` + first + `"},
		{"name":"` + second + `"}
	]}]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`data: {"response":{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"` + mapped + `","args":{}}},
		{"functionResponse":{"name":"` + mapped + `","response":{"result":"ok"}}}
	]}}]}}`)

	results := convertAntigravityResponseToGemini(
		util.WithAlt(context.Background(), ""),
		"gemini-3-flash",
		original,
		nil,
		responseJSON,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("output count = %d, want 1", len(results))
	}
	if got := gjson.GetBytes(results[0], "candidates.0.content.parts.0.functionCall.name").String(); got != second {
		t.Fatalf("stream functionCall.name = %q, want %q. Output: %s", got, second, results[0])
	}
	if got := gjson.GetBytes(results[0], "candidates.0.content.parts.1.functionResponse.name").String(); got != second {
		t.Fatalf("stream functionResponse.name = %q, want %q. Output: %s", got, second, results[0])
	}
}

func TestConvertAntigravityResponseToGeminiNonStreamRestoresDisambiguatedNames(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[{"functionDeclarations":[
		{"name":"` + first + `"},
		{"name":"` + second + `"}
	]}]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"` + mapped + `","args":{}}},
		{"functionResponse":{"name":"` + mapped + `","response":{"result":"ok"}}}
	]}}]}}`)

	output := convertAntigravityResponseToGeminiNonStream(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "candidates.0.content.parts.0.functionCall.name").String(); got != second {
		t.Fatalf("functionCall.name = %q, want %q. Output: %s", got, second, output)
	}
	if got := gjson.GetBytes(output, "candidates.0.content.parts.1.functionResponse.name").String(); got != second {
		t.Fatalf("functionResponse.name = %q, want %q. Output: %s", got, second, output)
	}
}
