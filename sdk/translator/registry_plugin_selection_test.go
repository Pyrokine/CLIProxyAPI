package translator

import (
	"context"
	"testing"
)

type overridingResponseHooks struct {
	labeledPluginHooks
	calls int
	input string
}

func (h *overridingResponseHooks) TranslateResponse(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) ([]byte, bool) {
	h.calls++
	h.input = string(body)
	return []byte("plugin-result"), true
}

func TestPluginResponseTranslationPrecedesNative(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(
			name, func(t *testing.T) {
				registry := NewRegistry()
				nativeCalls := 0
				registry.Register(
					FormatOpenAI, FormatClaude, nil, ResponseTransform{
						Stream: func(context.Context, string, []byte, []byte, []byte, *any) [][]byte {
							nativeCalls++
							return [][]byte{[]byte("native-result")}
						},
						NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte {
							nativeCalls++
							return []byte("native-result")
						},
					},
				)
				hooks := &overridingResponseHooks{label: "preferred"}
				ctx := WithPluginHooks(t.Context(), hooks)
				var result []byte
				if stream {
					frames := registry.TranslateStream(
						ctx, FormatClaude, FormatOpenAI, "fixture", nil, nil, []byte("source"), nil,
					)
					if len(frames) != 1 {
						t.Fatalf("plugin response frame count = %d, want 1", len(frames))
					}
					result = frames[0]
				} else {
					result = registry.TranslateNonStream(
						ctx, FormatClaude, FormatOpenAI, "fixture", nil, nil, []byte("source"), nil,
					)
				}
				if hooks.calls != 1 || hooks.input != "source|preferred-before" || nativeCalls != 0 ||
					string(result) != "plugin-result|preferred-after" {
					t.Fatalf(
						"plugin calls=%d input=%q native calls=%d result=%q", hooks.calls, hooks.input, nativeCalls,
						result,
					)
				}
			},
		)
	}
}
