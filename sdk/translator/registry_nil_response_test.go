package translator

import (
	"context"
	"testing"
)

type nilResponseHooks struct {
	labeledPluginHooks
	afterCalled *bool
}

func (h nilResponseHooks) NormalizeResponseAfter(_ context.Context, _, _ Format, _ string, _, _, _ []byte, _ bool) []byte {
	*h.afterCalled = true
	return []byte(`{"success":true}`)
}

func TestNativeNilResponseSkipsPluginRecovery(t *testing.T) {
	for _, original := range []string{"{}", patchRequest} {
		for _, stream := range []bool{false, true} {
			t.Run(original+"/stream="+map[bool]string{false: "false", true: "true"}[stream], func(t *testing.T) {
				registry := NewRegistry()
				registry.Register(FormatOpenAIResponse, FormatOpenAI, nil, ResponseTransform{
					NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte { return nil },
					Stream:    func(context.Context, string, []byte, []byte, []byte, *any) [][]byte { return nil },
				})
				afterCalled := false
				ctx := WithPluginHooks(context.Background(), nilResponseHooks{afterCalled: &afterCalled})
				var param any
				if stream {
					if result := registry.TranslateStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(original), nil, []byte(`{}`), &param); len(result) != 0 {
						t.Fatalf("nil stream became a response: %q", result)
					}
				} else if result := registry.TranslateNonStream(ctx, FormatOpenAI, FormatOpenAIResponse, "fixture", []byte(original), nil, []byte(`{}`), &param); result != nil {
					t.Fatalf("nil native output became a response: %s", result)
				}
				if afterCalled {
					t.Fatal("plugin recovery was called after nil native output")
				}
			})
		}
	}
}
