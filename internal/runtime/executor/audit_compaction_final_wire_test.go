package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditCompactionFinalWireAndCapsuleModel(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "claude-sonnet-4-6"} {
		for _, stream := range []bool{false, true} {
			for _, rule := range []string{"none", "override", "filter"} {
				t.Run(
					fmt.Sprintf("%s/stream=%t/%s", model, stream, rule), func(t *testing.T) {
						cfg := &config.Config{}
						models := []config.PayloadModelRule{{Name: "*", Protocol: "antigravity"}}
						if rule != "none" {
							cfg.Payload.Override = []config.PayloadRule{
								{
									Models: models, Params: map[string]any{
										"model":                                 "nested-model-is-not-envelope-model",
										"tool_choice":                           "required",
										"toolConfig.functionCallingConfig.mode": "ANY",
										"toolConfig.functionCallingConfig.allowedFunctionNames": []string{
											"calculate",
										},
										"tools": []any{
											map[string]any{
												"functionDeclarations": []any{
													map[string]any{
														"name":       "calculate",
														"parameters": map[string]any{"type": "object"},
													},
												},
											}, map[string]any{"googleSearch": map[string]any{}},
										},
									},
								},
							}
						}
						if rule == "filter" {
							cfg.Payload.Filter = []config.PayloadFilterRule{
								{
									Models: models, Params: []string{"toolConfig"},
								},
							}
						}
						executor := NewAntigravityExecutor(cfg)
						auth := googlePayloadAuth("antigravity")
						req := cliproxyexecutor.Request{
							Model: model,
							Payload: []byte(
								(`{"model":"caller-body-model","tool_choice":"required","additional_tools":[{"` +
									`type":"function","name":"calculate","parameters":{"type":"object"}}],"` +
									`input":[{"role":"user","content":"long conversation"},{"type":"` +
									`compaction_trigger"}]}`),
							),
						}
						opts := antigravityCompactionTestOptions(
							cliproxyexecutor.Options{
								SourceFormat:   sdktranslator.FormatOpenAIResponse,
								ResponseFormat: sdktranslator.FormatOpenAIResponse,
							}, "synthetic-compaction-wire",
						)
						calls := 0
						var wire []byte
						ctx := util.WithRoundTripper(
							t.Context(), googlePayloadTransport(
								func(r *http.Request) (*http.Response, error) {
									calls++
									var err error
									wire, err = io.ReadAll(r.Body)
									if err != nil {
										return nil, err
									}
									body := antigravityCompactionSummaryResponse
									if strings.Contains(r.URL.Path, "streamGenerateContent") {
										body = "data: " + body + "\n\n"
									}
									return googlePayloadResponse(r, body), nil
								},
							),
						)
						var output []byte
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								output = append(output, chunk.Payload...)
							}
						} else {
							response, err := executor.Execute(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							output = response.Payload
						}
						if calls != 1 || gjson.GetBytes(wire, "model").String() != model {
							t.Fatalf("calls=%d wire=%s", calls, wire)
						}
						if gjson.GetBytes(
							wire, "request.toolConfig.functionCallingConfig.mode",
						).String() != "NONE" || gjson.GetBytes(
							wire, "request.toolConfig.functionCallingConfig.allowedFunctionNames",
						).Exists() || gjson.GetBytes(wire, "request.tool_choice").Exists() {
							t.Fatalf("summary tools enabled: %s", wire)
						}
						for _, tool := range gjson.GetBytes(wire, "request.tools").Array() {
							if !tool.Get("functionDeclarations").IsArray() ||
								tool.Get("googleSearch").Exists() {
								t.Fatalf("summary built-in tool enabled: %s", wire)
							}
						}
						if rule != "none" && gjson.GetBytes(
							wire, "request.tools.0.functionDeclarations.0.name",
						).String() != "calculate" {
							t.Fatalf("legal declaration lost: %s", wire)
						}
						text := string(output)
						start := strings.Index(text, "cpa-ag-compact-v1:")
						if start < 0 {
							t.Fatalf("capsule missing: %s", output)
						}
						capsule, _, _ := strings.Cut(text[start:], `"`)
						scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
						scope.Model = gjson.GetBytes(wire, "model").String()
						if summary, err := helps.UnsealAntigravityCompaction(
							capsule, scope,
						); err != nil || summary != "compact summary" {
							t.Fatalf("final wire model capsule=%q error=%v", summary, err)
						}
						scope.Model = "nested-model-is-not-envelope-model"
						if _, err := helps.UnsealAntigravityCompaction(capsule, scope); err == nil {
							t.Fatal("capsule accepted the nested configured model")
						}
					},
				)
			}
		}
	}
}

func TestAuditCompactionBindsAfterNativePayloadPreparation(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := googlePayloadAuth("antigravity")
	req := cliproxyexecutor.Request{Model: "gemini-2.5-flash"}
	opts := antigravityCompactionTestOptions(
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}, "synthetic-final-binding",
	)
	for _, summary := range []bool{false, true} {
		scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
		scope.Model = "stale-model-before-preparation"
		ctx := context.WithValue(
			t.Context(), antigravityCompactionContextKey{},
			antigravityCompactionRequestContext{scope: &scope, summary: summary},
		)
		wire, err := executor.prepareAntigravityRequestPayload(
			ctx,
			auth,
			req,
			opts,
			[]byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
			nil,
		)
		if summary {
			if err != nil || scope.Model != gjson.GetBytes(wire, "model").String() || scope.Model != req.Model {
				t.Fatalf("scope=%+v wire=%s error=%v", scope, wire, err)
			}
		} else if err == nil {
			t.Fatal("replay accepted a different final wire model")
		}
	}
}

func TestAuditSequentialCompactionRetainsReplayModelAuthority(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := googlePayloadAuth("antigravity")
	req := cliproxyexecutor.Request{Model: "gemini-2.5-flash"}
	opts := antigravityCompactionTestOptions(cliproxyexecutor.Options{}, "synthetic-sequential-final")
	scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
	ctx := context.WithValue(
		t.Context(),
		antigravityCompactionContextKey{},
		antigravityCompactionRequestContext{
			scope:       &scope,
			summary:     true,
			replayModel: "previous-capsule-model",
		},
	)
	if _, err := executor.prepareAntigravityRequestPayload(
		ctx, auth, req, opts,
		[]byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"previous summary"}]}]}}`), nil,
	); err == nil {
		t.Fatal("sequential summary discarded the previous capsule's final model authority")
	}
}
