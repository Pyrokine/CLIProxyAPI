package helps

import (
	"net/http"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestIsNativeCodexRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		source   sdktranslator.Format
		response sdktranslator.Format
		headers  http.Header
		want     bool
	}{
		{
			name:    "responses lite header",
			source:  sdktranslator.FormatCodex,
			headers: http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{" TrUe "}},
			want:    true,
		},
		{
			name:     "mixed native formats",
			source:   sdktranslator.FormatOpenAIResponse,
			response: sdktranslator.FormatCodex,
			headers:  http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}},
			want:     true,
		},
		{
			name:   "boolean metadata",
			body:   `{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true}}`,
			source: sdktranslator.FormatOpenAIResponse,
			want:   true,
		},
		{
			name:   "string metadata",
			body:   `{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":" TRUE "}}`,
			source: sdktranslator.FormatCodex,
			want:   true,
		},
		{
			name:    "missing lite marker",
			source:  sdktranslator.FormatCodex,
			headers: http.Header{},
		},
		{
			name:     "non native source",
			source:   sdktranslator.FormatOpenAI,
			response: sdktranslator.FormatOpenAIResponse,
			headers:  http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}},
		},
		{
			name:     "non native response",
			source:   sdktranslator.FormatCodex,
			response: sdktranslator.FormatOpenAI,
			headers:  http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}},
		},
		{
			name:    "false header",
			source:  sdktranslator.FormatCodex,
			headers: http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"false"}},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := IsNativeCodexRequest([]byte(testCase.body), cliproxyexecutor.Options{
				SourceFormat:   testCase.source,
				ResponseFormat: testCase.response,
				Headers:        testCase.headers,
			})
			if got != testCase.want {
				t.Fatalf("IsNativeCodexRequest() = %t, want %t", got, testCase.want)
			}
		})
	}
}
