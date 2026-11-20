package translator

import (
	"context"
	"testing"
)

func TestResponseTransformAllowsRegistration(t *testing.T) {
	for _, kind := range []string{"stream", "nonstream", "token-count"} {
		t.Run(
			kind, func(t *testing.T) {
				registry := NewRegistry()
				register := func() {
					if !registry.mu.TryLock() {
						t.Error("response callback holds the registry lock and would block registration")
						return
					}
					registry.mu.Unlock()
					registry.Register(
						FormatGemini, FormatOpenAI, func(_ string, body []byte, _ bool) []byte {
							return body
						}, ResponseTransform{},
					)
				}
				registry.Register(
					FormatOpenAI, FormatClaude, nil, ResponseTransform{
						Stream: func(context.Context, string, []byte, []byte, []byte, *any) [][]byte {
							register()
							return [][]byte{[]byte("translated")}
						},
						NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte {
							register()
							return []byte("translated")
						},
						TokenCount: func(context.Context, int64) []byte {
							register()
							return []byte("translated")
						},
					},
				)
				ctx := WithPluginHooks(t.Context(), nil)
				var result []byte
				switch kind {
				case "stream":
					frames := registry.TranslateStream(
						ctx, FormatClaude, FormatOpenAI, "fixture", nil, nil, []byte("source"), nil,
					)
					if len(frames) != 1 {
						t.Fatalf("response frame count = %d, want 1", len(frames))
					}
					result = frames[0]
				case "nonstream":
					result = registry.TranslateNonStream(
						ctx, FormatClaude, FormatOpenAI, "fixture", nil, nil, []byte("source"), nil,
					)
				case "token-count":
					result = registry.TranslateTokenCount(ctx, FormatClaude, FormatOpenAI, 1, []byte("source"))
				}
				if string(result) != "translated" || !registry.HasRequestTransformer(FormatGemini, FormatOpenAI) {
					t.Fatalf("callback result = %q, registration did not complete", result)
				}
			},
		)
	}
}
