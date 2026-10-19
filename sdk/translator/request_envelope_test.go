package translator

import (
	"context"
	"errors"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestRequestEnvelopePreservesSelectedModelAndContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "request-marker")
	for _, supported := range []bool{false, true} {
		modelInfo := &registry.ModelInfo{ID: "selected-model", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported}}
		translator := NewRegistry()
		translator.RegisterRequestEnvelope(FormatOpenAIResponse, FormatAntigravity,
			func(current context.Context, req RequestEnvelope) RequestEnvelope {
				if current.Value(contextKey{}) != "request-marker" || req.ModelInfo != modelInfo {
					t.Fatal("selected model or request context was lost")
				}
				req.Body = []byte(`{"selected":true}`)
				return req
			},
		)
		if !translator.HasRequestTransformer(FormatOpenAIResponse, FormatAntigravity) {
			t.Fatal("envelope-aware converter was not registered")
		}
		translator.RegisterRequestEnvelope(FormatOpenAIResponse, FormatAntigravity, nil)
		out, err := NewPipeline(translator).TranslateRequest(WithPluginHooks(ctx, nil), FormatOpenAIResponse, FormatAntigravity,
			RequestEnvelope{Model: "selected-model", ModelInfo: modelInfo, Body: []byte(`{}`)})
		if err != nil || out.Err != nil || out.ModelInfo != modelInfo || out.Format != FormatAntigravity ||
			string(out.Body) != `{"selected":true}` || *out.ModelInfo.NativeCapabilities.WebSearch != supported {
			t.Fatalf("envelope translation = %+v, err = %v", out, err)
		}
	}
}

func TestRequestEnvelopeRejectsWithoutPluginRecovery(t *testing.T) {
	failure := errors.New("synthetic request rejection")
	translator := NewRegistry()
	translator.RegisterRequestEnvelope(FormatOpenAIResponse, FormatAntigravity,
		func(_ context.Context, req RequestEnvelope) RequestEnvelope {
			req.Body = []byte(`{"must_not_succeed":true}`)
			req.Err = failure
			return req
		},
	)
	calls := 0
	ctx := WithPluginHooks(t.Context(), labeledPluginHooks{normalizeRequestCalls: &calls})
	out, err := NewPipeline(translator).TranslateRequest(ctx, FormatOpenAIResponse, FormatAntigravity,
		RequestEnvelope{Model: "selected-model", Body: []byte(`{}`)})
	if !errors.Is(err, failure) || !errors.Is(out.Err, failure) || out.Body != nil || calls != 0 {
		t.Fatalf("rejection = %+v, err = %v, normalizer calls = %d", out, err, calls)
	}
}
