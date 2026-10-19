package translator

import (
	"context"
	"testing"
)

func TestRegistryResponseCapabilities(t *testing.T) {
	stream := func(context.Context, string, []byte, []byte, []byte, *any) [][]byte { return nil }
	nonStream := func(context.Context, string, []byte, []byte, []byte, *any) []byte { return nil }
	tokenCount := func(context.Context, int64) []byte { return nil }
	for _, test := range []struct {
		name       string
		response   ResponseTransform
		wantAny    bool
		wantStream bool
	}{
		{name: "empty"},
		{name: "stream", response: ResponseTransform{Stream: stream}, wantAny: true, wantStream: true},
		{name: "non-stream", response: ResponseTransform{NonStream: nonStream}, wantAny: true},
		{name: "token-count", response: ResponseTransform{TokenCount: tokenCount}, wantAny: true},
		{name: "combined", response: ResponseTransform{Stream: stream, NonStream: nonStream, TokenCount: tokenCount}, wantAny: true, wantStream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			from, to := Format("response-capability-from"), Format("response-capability-to")
			registry.Register(from, to, func(_ string, body []byte, _ bool) []byte { return body }, test.response)
			if got := registry.HasResponseTransformer(from, to); got != test.wantAny {
				t.Fatalf("HasResponseTransformer() = %v, want %v", got, test.wantAny)
			}
			if got := registry.HasStreamResponseTransformer(from, to); got != test.wantStream {
				t.Fatalf("HasStreamResponseTransformer() = %v, want %v", got, test.wantStream)
			}
			if registry.HasResponseTransformer(from, Format("unregistered")) {
				t.Fatal("unregistered format was reported as a response transformer")
			}
		})
	}
}
