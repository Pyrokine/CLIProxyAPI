package translator

import (
	"context"
	"testing"
)

type labeledPluginHooks struct {
	label string
}

func (h labeledPluginHooks) NormalizeRequest(
	_ context.Context,
	_, _ Format,
	_ string,
	body []byte,
	_ bool,
) []byte {
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

	if got := string(registry.TranslateRequestWithContext(
		ctxA, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
	)); got != "request|a-request|request-base" {
		t.Fatalf("service A request = %q", got)
	}
	if got := string(registry.TranslateRequestWithContext(
		ctxB, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
	)); got != "request|b-request|request-base" {
		t.Fatalf("service B request = %q", got)
	}
	if got := string(registry.TranslateRequestWithContext(
		ctxNil, FormatOpenAI, FormatClaude, "model", []byte("request"), false,
	)); got != "request|request-base" {
		t.Fatalf("explicit nil request hooks inherited global hooks: %q", got)
	}

	if got := string(registry.TranslateNonStream(
		ctxA,
		FormatOpenAI,
		FormatClaude,
		"model",
		nil,
		nil,
		[]byte("response"),
		nil,
	)); got != "response|a-before|response-base|a-after" {
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
	if got := string(registry.TranslateNonStream(
		ctxNil,
		FormatOpenAI,
		FormatClaude,
		"model",
		nil,
		nil,
		[]byte("response"),
		nil,
	)); got != "response|response-base" {
		t.Fatalf("explicit nil response hooks inherited global hooks: %q", got)
	}
}
