package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditPayloadConditionsCompareExactJSONNumbers(t *testing.T) {
	for _, tc := range []struct {
		actual, expected string
		equal            bool
	}{
		{"9007199254740992", "9007199254740993", false},
		{"-9007199254740992", "-9007199254740993", false},
		{"9007199254740993", "9007199254740993", true},
		{"-9007199254740993", "-9007199254740993", true},
		{"1", "2", false},
		{"1.0", "1", true},
		{"1e0", "1.00", true},
	} {
		for _, notMatch := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/%s/not-match=%t", tc.actual, tc.expected, notMatch), func(t *testing.T) {
					model := config.PayloadModelRule{Name: "synthetic-model"}
					condition := []map[string]any{{"value": json.Number(tc.expected)}}
					if notMatch {
						model.NotMatch = condition
					} else {
						model.Match = condition
					}
					cfg := &config.Config{
						Payload: config.PayloadConfig{
							Override: []config.PayloadRule{
								{
									Models: []config.PayloadModelRule{model},
									Params: map[string]any{"configured_marker": true},
								},
							},
						},
					}
					payload := []byte(`{"value":` + tc.actual + `}`)
					out := applyPayloadConfigWithRequest(
						cfg, "synthetic-model", "openai", "openai", "", payload, payload, "", "", nil, "openai",
					)
					if got := gjson.GetBytes(out, "configured_marker").Exists(); got != (tc.equal != notMatch) {
						t.Fatalf(
							"rule matched=%t equal=%t not-match=%t payload=%s",
							got,
							tc.equal,
							notMatch,
							out,
						)
					}
				},
			)
		}
	}
}

func TestAuditXAIDispatcherPreservesExactArgumentSnapshots(t *testing.T) {
	for _, tc := range []struct {
		first, second string
		conflict      bool
	}{
		{"9007199254740992", "9007199254740993", true},
		{"-9007199254740992", "-9007199254740993", true},
		{"9007199254740993", "9007199254740993", false},
		{"-9007199254740993", "-9007199254740993", false},
		{"1", "2", true},
		{"1", "1.0", false},
	} {
		t.Run(
			tc.first+"/"+tc.second, func(t *testing.T) {
				normalizer := xaiPatchNamespaceNormalizer(t)
				events := normalizer.normalize(
					[]byte(
						`{"type":"response.output_item.added","output_index":2,"item":{"type":` +
							`"function_call","id":"item-1","name":` +
							`"editor"}}`,
					),
				)
				for _, number := range []string{tc.first, tc.second} {
					wrapper := `{"name":"lookup","arguments":{"value":` + number + `}}`
					events = append(
						events, normalizer.normalize(
							[]byte(fmt.Sprintf(
								`{"type":"response.function_call_arguments.done","item_id":"item-1","arguments":%q}`,
								wrapper,
							)),
						)...,
					)
				}
				events = append(
					events,
					normalizer.normalize(
						[]byte(`{"type":"response.output_item.done","output_index":2,"item":{"type":"f`+
							`unction_call","id":"item-1","call_id":"call-1","name":"lookup"}}`),
					)...,
				)
				events = append(
					events,
					normalizer.normalize([]byte(`{"type":"response.completed","response":{"output":[]}}`))...,
				)
				failed, completed := 0, 0
				for _, event := range events {
					switch gjson.GetBytes(event, "type").String() {
					case "response.failed":
						failed++
					case "response.completed":
						completed++
					}
				}
				if tc.conflict && (failed != 1 || completed != 0) ||
					!tc.conflict && (failed != 0 || completed != 1) {
					t.Fatalf(
						"conflict=%t failed=%d completed=%d events=%q",
						tc.conflict,
						failed,
						completed,
						events,
					)
				}
			},
		)
	}
}

func TestAuditPublicCodexAndMetaCountStringInput(t *testing.T) {
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	for name, executor := range map[string]auth.ProviderExecutor{
		"codex": NewCodexAutoExecutor(cfg), "meta": NewMetaExecutor(cfg),
	} {
		t.Run(
			name, func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(*http.Request) (*http.Response, error) {
							calls++
							return nil, fmt.Errorf("local count dispatched an upstream request")
						},
					),
				)
				credential := &auth.Auth{
					Provider: name, Attributes: map[string]string{
						auth.AttributeAuthKind: auth.AuthKindAPIKey, auth.AttributeAPIKey: "synthetic-key",
					},
				}
				opts := core.Options{
					SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
				}
				counts := make([]int64, 0, 2)
				for _, input := range []string{
					`"hello world counting tokens"`,
					`[{"type":"message","role":"user","content":[{"type":"input_text",` +
						`"text":"hello world counting ` +
						`tokens"}]}]`,
				} {
					result, err := executor.CountTokens(
						ctx,
						credential,
						core.Request{
							Model:   "gpt-5.4",
							Payload: []byte(`{"model":"gpt-5.4","input":` + input + `}`),
						},
						opts,
					)
					if err != nil {
						t.Fatal(err)
					}
					counts = append(counts, gjson.GetBytes(result.Payload, "response.usage.input_tokens").Int())
				}
				if calls != 0 || counts[0] == 0 || counts[0] != counts[1] {
					t.Fatalf("dispatches=%d string/array counts=%v", calls, counts)
				}
			},
		)
	}
}

func TestAuditClaudeParentThinkingRulesRepairOriginalValidation(t *testing.T) {
	for _, kind := range []string{"override", "override-raw", "filter", "leaf-control"} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
					models := []config.PayloadModelRule{{Name: "claude-sonnet-5-5", Protocol: "claude"}}
					cfg := &config.Config{}
					switch kind {
					case "override":
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{"thinking": map[string]any{"type": "disabled"}},
							},
						}
					case "override-raw":
						cfg.Payload.OverrideRaw = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{"thinking": `{"type":"disabled"}`},
							},
						}
					case "filter":
						cfg.Payload.Filter = []config.PayloadFilterRule{
							{Models: models, Params: []string{"thinking"}},
						}
					case "leaf-control":
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{"thinking.type": "disabled"},
							},
						}
					}
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(request.Body)
								if err != nil {
									return nil, err
								}
								if gjson.GetBytes(body, "thinking.type").String() == "enabled" {
									t.Errorf("invalid thinking survived configured repair: %s", body)
								}
								contentType, response := "application/json", claudeReplayTextResponse(
									"synthetic",
								)
								if stream {
									contentType, response = "text/event-stream", claudeReplayTextStream(
										"synthetic",
									)
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
									Body: io.NopCloser(strings.NewReader(response)), Request: request,
								}, nil
							},
						),
					)
					executor := NewClaudeExecutor(cfg)
					payload := []byte(
						`{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user` +
							`","content":"synthetic"}],"thinking":{"type":"enabled","budget_tokens"` +
							`:1024}}`,
					)
					req := core.Request{Model: "claude-sonnet-5-5", Payload: payload}
					opts := core.Options{
						SourceFormat:   translator.FormatClaude,
						ResponseFormat: translator.FormatClaude,
					}
					credential := claudeReplayTestAuth("http://127.0.0.1:1")
					var err error
					if stream {
						var result *core.StreamResult
						result, err = executor.ExecuteStream(ctx, credential, req, opts)
						if err == nil && drainClaudeReplayStream(t, result) {
							t.Fatal("stream failed")
						}
					} else {
						_, err = executor.Execute(ctx, credential, req, opts)
					}
					if err != nil || calls != 1 {
						t.Fatalf("dispatches=%d error=%v", calls, err)
					}
				},
			)
		}
	}
}
