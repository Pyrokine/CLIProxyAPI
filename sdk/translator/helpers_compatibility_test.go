package translator_test

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestPublishedFormatNameCapabilityHelpers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		request  translator.RequestTransform
		response translator.ResponseTransform
		want     [4]bool
	}{
		{name: "empty"},
		{
			name:    "request",
			request: func(_ string, body []byte, _ bool) []byte { return body },
			want:    [4]bool{true, false, false, false},
		},
		{
			name: "stream",
			response: translator.ResponseTransform{
				Stream: func(context.Context, string, []byte, []byte, []byte, *any) [][]byte { return nil },
			},
			want: [4]bool{false, true, true, false},
		},
		{
			name: "nonstream",
			response: translator.ResponseTransform{
				NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte { return nil },
			},
			want: [4]bool{false, true, false, true},
		},
		{
			name: "token-count",
			response: translator.ResponseTransform{
				TokenCount: func(context.Context, int64) []byte { return nil },
			},
			want: [4]bool{false, true, false, false},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				from := translator.Format("published-source-" + t.Name())
				to := translator.Format("published-target-" + t.Name())
				translator.Register(from, to, tc.request, tc.response)
				got := [4]bool{
					translator.HasRequestTransformerByFormatName(from, to),
					translator.HasResponseTransformerByFormatName(from, to),
					translator.HasStreamResponseTransformerByFormatName(from, to),
					translator.HasNonStreamResponseTransformerByFormatName(from, to),
				}
				if got != tc.want {
					t.Fatalf("capabilities = %v, want %v", got, tc.want)
				}
				if translator.HasRequestTransformerByFormatName(to, from) ||
					translator.HasResponseTransformerByFormatName(to, from) ||
					translator.HasStreamResponseTransformerByFormatName(to, from) ||
					translator.HasNonStreamResponseTransformerByFormatName(to, from) {
					t.Fatal("capability lookup reversed the registered translation direction")
				}
			},
		)
	}
}
