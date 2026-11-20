package gemini

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRestoreUsageMetadata(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{
			name: "cpaUsageMetadata renamed to usageMetadata",
			input: []byte(
				`{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":` +
					`100,"candidatesTokenCount":` +
					`200}}`,
			),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":` +
				`100,"candidatesTokenCount":` +
				`200}}`,
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
			name: "cpaUsageMetadata restored in response",
			input: []byte(
				`{"response":{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100}}}`,
			),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
		{
			name: "usageMetadata preserved",
			input: []byte(
				`{"response":{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}}`,
			),
			expected: `{"modelVersion":"gemini-3-pro","usageMetadata":{"promptTokenCount":100}}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := convertAntigravityResponseToGeminiNonStream(
					context.Background(),
					"",
					nil,
					nil,
					tt.input,
					nil,
				)
				if string(result) != tt.expected {
					t.Errorf(
						"ConvertAntigravityResponseToGeminiNonStream() = %s, want %s",
						string(result),
						tt.expected,
					)
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
			name: "cpaUsageMetadata restored in streaming response",
			input: []byte(
				`data: {"response":{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100}}}`,
			),
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
					t.Errorf(
						"ConvertAntigravityResponseToGemini() = %s, want %s",
						string(results[0]),
						tt.expected,
					)
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

func TestConvertAntigravityResponseToGeminiRestoresExternalToolNameChain(t *testing.T) {
	const clientName = "external_external_read_file"
	const upstreamName = "external_external_external_read_file"
	original := []byte(`{"tools":[{"functionDeclarations":[{"name":"` + clientName + `"}]}]}`)
	response := `{"response":{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"` + upstreamName + `","args":{}}},
		{"functionResponse":{"name":"` + upstreamName + `","response":{"result":"ok"}}}
	]}}]}}`

	stream := convertAntigravityResponseToGemini(
		util.WithAlt(context.Background(), ""),
		"gemini-3-flash",
		original,
		nil,
		[]byte("data: "+response),
		nil,
	)
	if len(stream) != 1 {
		t.Fatalf("stream output count = %d, want 1", len(stream))
	}
	for _, path := range []string{
		"candidates.0.content.parts.0.functionCall.name",
		"candidates.0.content.parts.1.functionResponse.name",
	} {
		if got := gjson.GetBytes(stream[0], path).String(); got != clientName {
			t.Fatalf("stream %s = %q, want %q; output=%s", path, got, clientName, stream[0])
		}
	}

	nonStream := convertAntigravityResponseToGeminiNonStream(
		context.Background(),
		"gemini-3-flash",
		original,
		nil,
		[]byte(response),
		nil,
	)
	for _, path := range []string{
		"candidates.0.content.parts.0.functionCall.name",
		"candidates.0.content.parts.1.functionResponse.name",
	} {
		if got := gjson.GetBytes(nonStream, path).String(); got != clientName {
			t.Fatalf("non-stream %s = %q, want %q; output=%s", path, got, clientName, nonStream)
		}
	}
}

func TestAntigravityAltArraySDKPreservesResponses(t *testing.T) {
	request := []byte(`{"tools":[{"functionDeclarations":[{"name":"1lookup"}]}]}`)
	alias := util.SanitizedFunctionNameMap(request)["1lookup"]
	body := []byte(
		`[{"response":{"candidates":[{"content":{"parts":[{"functionCall":` +
			`{"name":` +
			`"` +
			alias +
			(`","args":{}}}]},"finishReason":"MAX_TOKENS"}],"cpaUsageMetadata":{"tot` +
				`alTokenCount":9}}},{"response":{"candidates":[{"content":{"parts":[{"t` +
				`ext":"second"}]}}],"usageMetadata":{"totalTokenCount":4}}}]`),
	)
	var state any
	out := sdktranslator.Default().TranslateStream(
		util.WithAlt(context.Background(), "json"), sdktranslator.FormatAntigravity, sdktranslator.FormatGemini,
		"fixture", request, nil, body, &state,
	)
	if len(out) != 1 || gjson.GetBytes(out[0], "#").Int() != 2 {
		t.Fatalf("array responses missing: %s", out)
	}
	for path, want := range map[string]string{
		"0.candidates.0.content.parts.0.functionCall.name": "1lookup", "0.candidates.0.finishReason": "MAX_TOKENS",
		"0.usageMetadata.totalTokenCount": "9", "1.candidates.0.content.parts.0.text": "second",
		"1.usageMetadata.totalTokenCount": "4",
	} {
		if got := gjson.GetBytes(out[0], path).String(); got != want {
			t.Fatalf("%s=%q want %q; %s", path, got, want, out[0])
		}
	}
	if gjson.GetBytes(out[0], "1.candidates.0.finishReason").Exists() || gjson.GetBytes(
		out[0], "0.cpaUsageMetadata",
	).Exists() {
		t.Fatalf("finish reason invented or internal usage retained: %s", out[0])
	}
}
