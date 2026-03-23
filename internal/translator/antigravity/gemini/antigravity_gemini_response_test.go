package gemini

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
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
				`{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100,"candidatesTokenCount":200}}`,
			),
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
			name: "cpaUsageMetadata restored in response",
			input: []byte(
				`{"response":{"modelVersion":"gemini-3-pro","cpaUsageMetadata":{"promptTokenCount":100}}}`,
			),
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
				if result != tt.expected {
					t.Errorf("convertAntigravityResponseToGeminiNonStream() = %s, want %s", result, tt.expected)
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
				if results[0] != tt.expected {
					t.Errorf("convertAntigravityResponseToGemini() = %s, want %s", results[0], tt.expected)
				}
			},
		)
	}
}
