package gemini

import (
	"context"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

const (
	geminiResponseSignatureA = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	geminiResponseSignatureB = "CgQBBQYH"
)

func TestConvertClaudeResponseToGeminiStreamsAggregatedThinkingSignature(t *testing.T) {
	tests := []struct {
		name          string
		events        []string
		wantSignature string
	}{
		{
			name: "content block signature and deltas",
			events: []string{
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"gemini#` + geminiResponseSignatureA[:24] + `"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureA[24:48] + `"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureA[48:] + `"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			},
			wantSignature: geminiResponseSignatureA,
		},
		{
			name: "signature delta fragments",
			events: []string{
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureB[:4] + `"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureB[4:] + `"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			},
			wantSignature: geminiResponseSignatureB,
		},
		{
			name: "foreign signature maps to bypass",
			events: []string{
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EgQK"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"AggM"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			},
			wantSignature: signature.GeminiSkipThoughtSignatureValidator,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var param any
			var chunks [][]byte
			for index, event := range testCase.events {
				chunks = ConvertClaudeResponseToGemini(
					context.Background(),
					"gemini-3-pro",
					nil,
					nil,
					[]byte(event),
					&param,
				)
				if index < len(testCase.events)-1 && len(chunks) != 0 {
					t.Fatalf("event %d emitted %d chunks before signature completion", index, len(chunks))
				}
			}
			if len(chunks) != 1 {
				t.Fatalf("final chunk count = %d, want 1", len(chunks))
			}
			part := gjson.GetBytes(chunks[0], "candidates.0.content.parts.0")
			if !part.Get("thought").Bool() || part.Get("thoughtSignature").String() != testCase.wantSignature {
				t.Fatalf("unexpected thought part: %s", part.Raw)
			}
		})
	}
}

func TestConvertClaudeResponseToGeminiNonStreamAggregatesThinkingSignatureFragments(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"` + geminiResponseSignatureA[:20] + `"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private reasoning"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureA[20:50] + `"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + geminiResponseSignatureA[50:] + `"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}, "\n"))

	output := ConvertClaudeResponseToGeminiNonStream(
		context.Background(),
		"gemini-3-pro",
		nil,
		nil,
		raw,
		nil,
	)
	parts := gjson.GetBytes(output, "candidates.0.content.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("part count = %d, want 1; output: %s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "private reasoning" {
		t.Fatalf("thinking text = %q, want private reasoning", got)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != geminiResponseSignatureA {
		t.Fatalf("thoughtSignature = %q, want aggregated signature %q", got, geminiResponseSignatureA)
	}
}

func TestConvertClaudeResponseToGeminiPreservesToolEnvelope(t *testing.T) {
	var param any
	start := []byte(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"native-call-id","name":"lookup","signature":"` + geminiResponseSignatureA + `"}}`)
	if chunks := ConvertClaudeResponseToGemini(
		context.Background(), "gemini-3-pro", nil, nil, start, &param,
	); len(chunks) != 0 {
		t.Fatalf("tool start emitted %d chunks, want 0", len(chunks))
	}
	delta := []byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"status\"}"}}`)
	if chunks := ConvertClaudeResponseToGemini(
		context.Background(), "gemini-3-pro", nil, nil, delta, &param,
	); len(chunks) != 0 {
		t.Fatalf("tool delta emitted %d chunks, want 0", len(chunks))
	}
	chunks := ConvertClaudeResponseToGemini(
		context.Background(),
		"gemini-3-pro",
		nil,
		nil,
		[]byte(`data: {"type":"content_block_stop","index":2}`),
		&param,
	)
	if len(chunks) != 1 {
		t.Fatalf("tool stop chunk count = %d, want 1", len(chunks))
	}
	part := gjson.GetBytes(chunks[0], "candidates.0.content.parts.0")
	if got := part.Get("functionCall.id").String(); got != "native-call-id" {
		t.Fatalf("functionCall.id = %q, want native-call-id", got)
	}
	if got := part.Get("thoughtSignature").String(); got != geminiResponseSignatureA {
		t.Fatalf("thoughtSignature = %q, want %q", got, geminiResponseSignatureA)
	}
	if got := part.Get("functionCall.args.query").String(); got != "status" {
		t.Fatalf("functionCall.args.query = %q, want status", got)
	}
}
