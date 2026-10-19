package translator

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCheckedRequestRejectsWithoutCallingPluginHooks(t *testing.T) {
	registry := NewRegistry()
	refused := errors.New("unsupported content part: input_file")
	registry.RegisterCheckedRequest(FormatOpenAIResponse, FormatClaude,
		func(string, []byte, bool) ([]byte, error) {
			return []byte(`{"partial":true}`), refused
		})
	called := false
	ctx := WithPluginHooks(context.Background(), summaryPluginHooks{
		normalizeRequest: func(body []byte) []byte {
			called = true
			return body
		},
	})
	req := registry.TranslateRequestEnvelope(ctx, FormatOpenAIResponse, FormatClaude, RequestEnvelope{
		Model: "model", Body: []byte(`{"input":[]}`),
	})
	if !errors.Is(req.Err, refused) || req.Body != nil || called {
		t.Fatalf("TranslateRequestEnvelope() = %+v, plugin called = %v", req, called)
	}
	body, err := registry.TranslateRequestChecked(ctx, FormatOpenAIResponse, FormatClaude, "model", []byte(`{}`), false)
	if !errors.Is(err, refused) || body != nil {
		t.Fatalf("TranslateRequestChecked() = %q, %v", body, err)
	}
	if legacy := registry.TranslateRequestWithContext(ctx, FormatOpenAIResponse, FormatClaude, "model", []byte(`{}`), false); legacy != nil {
		t.Fatalf("legacy translation returned rejected content: %q", legacy)
	}
	pipeline := NewPipeline(registry)
	translated, err := pipeline.TranslateRequest(ctx, FormatOpenAIResponse, FormatClaude, RequestEnvelope{
		Model: "model", Body: []byte(`{}`),
	})
	if !errors.Is(err, refused) || translated.Body != nil {
		t.Fatalf("pipeline returned rejected content: %+v, %v", translated, err)
	}
}

func TestCheckedRequestPreservesLegacyResponseAndSuccessfulTranslation(t *testing.T) {
	registry := NewRegistry()
	registry.Register(FormatClaude, FormatOpenAI, nil, ResponseTransform{
		NonStream: func(_ context.Context, _ string, _, _, _ []byte, _ *any) []byte {
			return []byte(`{"reply":true}`)
		},
	})
	registry.RegisterCheckedRequest(FormatClaude, FormatOpenAI,
		func(_ string, _ []byte, _ bool) ([]byte, error) {
			return []byte(`{"translated":true}`), nil
		})
	body, err := registry.TranslateRequestChecked(context.Background(), FormatClaude, FormatOpenAI, "model", []byte(`{}`), false)
	if err != nil || !bytes.Equal(body, []byte(`{"translated":true}`)) {
		t.Fatalf("checked translation = %s, %v", body, err)
	}
	resp := registry.TranslateNonStream(context.Background(), FormatOpenAI, FormatClaude, "model", nil, body, nil, nil)
	if !bytes.Equal(resp, []byte(`{"reply":true}`)) {
		t.Fatalf("response registration replaced: %s", resp)
	}
}
