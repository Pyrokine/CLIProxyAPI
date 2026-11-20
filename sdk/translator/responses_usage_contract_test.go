package translator

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

type usageDetailsPluginHooks struct {
	labeledPluginHooks
	payload     []byte
	takeover    bool
	afterCalls  int
	replaceBody bool
}

func (*usageDetailsPluginHooks) NormalizeResponseBefore(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	return body
}

func (h *usageDetailsPluginHooks) TranslateResponse(
	context.Context,
	Format,
	Format,
	string,
	[]byte,
	[]byte,
	[]byte,
	bool,
) ([]byte, bool) {
	return h.payload, h.takeover
}

func (h *usageDetailsPluginHooks) NormalizeResponseAfter(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	h.afterCalls++
	if h.replaceBody {
		return h.payload
	}
	return body
}

func TestResponsesUsageDetailsCoverEveryRegistryExit(t *testing.T) {
	for _, tc := range []struct {
		name, body, usage string
	}{
		{"missing", `{"object":"response","usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100}}`, "usage"},
		{
			"null-empty",
			`{"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100,"inp` +
				`ut_tokens_details":null,"output_tokens_details":{}}}`,
			"usage",
		},
		{
			"scalar-buckets",
			`{"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100,"inp` +
				`ut_tokens_details":[],"output_tokens_details":"unknown"}}`,
			"usage",
		},
		{
			"existing",
			`{"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100,"input_tokens_details":{"` +
				`cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`,
			"usage",
		},
		{
			"nested",
			`{"type":"response.completed","response":{"usage":{"input_tokens":84,` +
				`"output_tokens":16,"total_tokens":` +
				`100}}}`,
			"response.usage",
		},
		{
			"data-substring",
			`{"output":[{"text":"data:image/png;base64,synthetic"}],"usage":{"input` +
				`_tokens":84,"output_tokens":16,"total_tokens":100}}`,
			"usage",
		},
		{
			"compaction",
			`{"object":"response.compaction","usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100}}`, "",
		},
		{"no-usage", `{"type":"response.output_text.delta","delta":"synthetic"}`, ""},
		{"done", `[DONE]`, ""},
	} {
		for _, mode := range []string{"native", "passthrough", "plugin", "after-normalizer"} {
			for _, to := range []Format{FormatOpenAIResponse, FormatCodex, FormatOpenAI} {
				for _, stream := range []bool{false, true} {
					t.Run(
						fmt.Sprintf("%s/%s/%s/stream=%t", tc.name, mode, to, stream), func(t *testing.T) {
							payload := []byte(tc.body)
							if stream {
								payload = append(
									append([]byte("event: synthetic\ndata: "), payload...), []byte("\n\n")...,
								)
							}
							registry := NewRegistry()
							nativeCalls := 0
							if mode != "passthrough" {
								registry.Register(
									to, FormatClaude, nil, ResponseTransform{
										Stream: func(context.Context, string, []byte, []byte, []byte, *any) [][]byte {
											nativeCalls++
											return [][]byte{payload}
										},
										NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte {
											nativeCalls++
											return payload
										},
									},
								)
							}
							ctx := WithPluginHooks(t.Context(), nil)
							hooks := &usageDetailsPluginHooks{
								payload: payload, takeover: mode == "plugin", replaceBody: mode == "after-normalizer",
							}
							if mode == "plugin" || mode == "after-normalizer" {
								ctx = WithPluginHooks(ctx, hooks)
							}
							var got []byte
							if stream {
								frames := registry.TranslateStream(
									ctx, FormatClaude, to, "synthetic", nil, nil, payload, nil,
								)
								if len(frames) != 1 {
									t.Fatalf("frame count=%d", len(frames))
								}
								got = frames[0]
							} else {
								got = registry.TranslateNonStream(
									ctx, FormatClaude, to, "synthetic", nil, nil, payload, nil,
								)
							}
							wantNative := 0
							if mode == "native" || mode == "after-normalizer" {
								wantNative = 1
							}
							if nativeCalls != wantNative ||
								((mode == "plugin" || mode == "after-normalizer") && hooks.afterCalls != 1) {
								t.Fatalf(
									"native calls=%d want=%d after calls=%d",
									nativeCalls,
									wantNative,
									hooks.afterCalls,
								)
							}
							if tc.usage == "" || to == FormatOpenAI {
								if !bytes.Equal(got, payload) {
									t.Fatalf("unrelated payload changed: %s", got)
								}
								return
							}
							if stream {
								prefix, suffix := []byte("event: synthetic\ndata: "), []byte("\n\n")
								if !bytes.HasPrefix(got, prefix) || !bytes.HasSuffix(got, suffix) {
									t.Fatalf("SSE framing changed: %q", got)
								}
								got = bytes.TrimSuffix(bytes.TrimPrefix(got, prefix), suffix)
							}
							usage := gjson.GetBytes(got, tc.usage)
							for _, field := range []string{
								"input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens",
							} {
								value := usage.Get(field)
								want := gjson.Get(tc.body, tc.usage+"."+field).Int()
								if !value.Exists() || value.Int() != want {
									t.Fatalf("%s=%s want=%d: %s", field, value.Raw, want, got)
								}
							}
							if usage.Get("input_tokens").Int() != 84 ||
								usage.Get("output_tokens").Int() != 16 ||
								usage.Get("total_tokens").Int() != 100 {
								t.Fatalf("original token totals changed: %s", got)
							}
						},
					)
				}
			}
		}
	}
}
