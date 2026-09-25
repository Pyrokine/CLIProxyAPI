package translator

import (
	"context"
	"testing"
)

type labeledPluginHooks struct {
	label                 string
	normalizeRequestCalls *int
}

func (h labeledPluginHooks) NormalizeRequest(
	_ context.Context,
	_, _ Format,
	_ string,
	body []byte,
	_ bool,
) []byte {
	if h.normalizeRequestCalls != nil {
		(*h.normalizeRequestCalls)++
	}
	return append(append([]byte(nil), body...), []byte("|"+h.label+"-request")...)
}

func (labeledPluginHooks) TranslateRequest(
	context.Context,
	Format,
	Format,
	string,
	[]byte,
	bool,
) ([]byte, bool) {
	return nil, false
}

func (h labeledPluginHooks) NormalizeResponseBefore(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	return append(append([]byte(nil), body...), []byte("|"+h.label+"-before")...)
}

func (labeledPluginHooks) TranslateResponse(
	context.Context,
	Format,
	Format,
	string,
	[]byte,
	[]byte,
	[]byte,
	bool,
) ([]byte, bool) {
	return nil, false
}

func (h labeledPluginHooks) NormalizeResponseAfter(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	return append(append([]byte(nil), body...), []byte("|"+h.label+"-after")...)
}

func TestPluginHooksAreIsolatedByContext(t *testing.T) {
	SetPluginHooks(labeledPluginHooks{label: "global"})
	t.Cleanup(func() { SetPluginHooks(nil) })

	registry := NewRegistry()
	registry.Register(
		FormatOpenAI,
		FormatClaude,
		func(_ string, body []byte, _ bool) []byte {
			return append(append([]byte(nil), body...), []byte("|request-base")...)
		},
		ResponseTransform{},
	)
	registry.Register(
		FormatClaude,
		FormatOpenAI,
		nil,
		ResponseTransform{
			Stream: func(
				_ context.Context,
				_ string,
				_, _, body []byte,
				_ *any,
			) [][]byte {
				return [][]byte{append(append([]byte(nil), body...), []byte("|stream-base")...)}
			},
			NonStream: func(
				_ context.Context,
				_ string,
				_, _, body []byte,
				_ *any,
			) []byte {
				return append(append([]byte(nil), body...), []byte("|response-base")...)
			},
		},
	)

	ctxA := WithPluginHooks(context.Background(), labeledPluginHooks{label: "a"})
	ctxB := WithPluginHooks(context.Background(), labeledPluginHooks{label: "b"})
	ctxNil := WithPluginHooks(context.Background(), nil)

	if got := string(
		registry.TranslateRequestWithContext(
			ctxA, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
		),
	); got != "request|request-base|a-request" {
		t.Fatalf("service A request = %q", got)
	}
	if got := string(
		registry.TranslateRequestWithContext(
			ctxB, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
		),
	); got != "request|request-base|b-request" {
		t.Fatalf("service B request = %q", got)
	}
	if got := string(
		registry.TranslateRequestWithContext(
			ctxNil, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
		),
	); got != "request|request-base" {
		t.Fatalf("explicit nil request hooks inherited global hooks: %q", got)
	}

	if got := string(
		registry.TranslateNonStream(
			ctxA,
			FormatOpenAI,
			FormatClaude,
			"model",
			nil,
			nil,
			[]byte("response"),
			nil,
		),
	); got != "response|a-before|response-base|a-after" {
		t.Fatalf("service A response = %q", got)
	}
	frames := registry.TranslateStream(
		ctxB,
		FormatOpenAI,
		FormatClaude,
		"model",
		nil,
		nil,
		[]byte("stream"),
		nil,
	)
	if len(frames) != 1 || string(frames[0]) != "stream|b-before|stream-base|b-after" {
		t.Fatalf("service B stream = %q", frames)
	}
	if got := string(
		registry.TranslateNonStream(
			ctxNil,
			FormatOpenAI,
			FormatClaude,
			"model",
			nil,
			nil,
			[]byte("response"),
			nil,
		),
	); got != "response|response-base" {
		t.Fatalf("explicit nil response hooks inherited global hooks: %q", got)
	}
}

func TestHasPluginHooksUsesRequestScopedOverride(t *testing.T) {
	SetPluginHooks(nil)
	t.Cleanup(func() { SetPluginHooks(nil) })

	if HasPluginHooks(context.Background()) {
		t.Fatal("hooks reported present without global or request hooks")
	}
	requestContext := WithPluginHooks(context.Background(), labeledPluginHooks{label: "request"})
	if !HasPluginHooks(requestContext) {
		t.Fatal("request-scoped hooks were not detected")
	}

	SetPluginHooks(labeledPluginHooks{label: "global"})
	//lint:ignore SA1012 This assertion verifies nil context falls back to package-level hooks.
	if !HasPluginHooks(context.Background()) || !HasPluginHooks(nil) {
		t.Fatal("global hooks were not detected")
	}
	if HasPluginHooks(WithPluginHooks(context.Background(), nil)) {
		t.Fatal("explicit nil request hooks did not override global hooks")
	}
}

func TestNormalizeRequestRunsExactlyOncePerTranslationPath(t *testing.T) {
	calls := 0
	hooks := labeledPluginHooks{label: "counted", normalizeRequestCalls: &calls}
	ctx := WithPluginHooks(context.Background(), hooks)
	registry := NewRegistry()
	registry.Register(
		FormatOpenAI,
		FormatClaude,
		func(_ string, body []byte, _ bool) []byte { return body },
		ResponseTransform{},
	)

	registry.TranslateRequestWithContext(ctx, FormatOpenAI, FormatClaude, "model", []byte(`{}`), false)
	if calls != 1 {
		t.Fatalf("registered path NormalizeRequest calls = %d, want 1", calls)
	}
	registry.TranslateRequestWithContext(ctx, FormatGemini, FormatClaude, "model", []byte(`{}`), false)
	if calls != 2 {
		t.Fatalf("fallback path total NormalizeRequest calls = %d, want 2", calls)
	}
}
