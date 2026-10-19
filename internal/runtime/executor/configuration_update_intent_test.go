package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type executorConfigurationUpdateHooks struct {
	executorTranslatorHooks
	effort string
}

func (h *executorConfigurationUpdateHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.requestCalls.Add(1)
	if gjson.GetBytes(body, "input.0.type").String() != "configuration_update" {
		return body
	}
	if h.effort == "" {
		out, _ := sjson.DeleteBytes(body, "input.0")
		return out
	}
	out, _ := sjson.SetBytes(body, "input.0.reasoning.effort", h.effort)
	return out
}

func TestResponsesExecutorsPreserveConfigurationUpdateNormalizerIntent(t *testing.T) {
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hi"}]}`
	for _, provider := range []string{"codex", "xai"} {
		for _, streaming := range []bool{false, true} {
			for _, supported := range []bool{false, true} {
				for _, effort := range []string{"", "high"} {
					t.Run(fmt.Sprintf("%s/stream=%v/native=%v/edit=%s", provider, streaming, supported, effort), func(t *testing.T) {
						hooks := &executorConfigurationUpdateHooks{effort: effort}
						ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
						request := cliproxyexecutor.Request{
							Model: "synthetic-route", Payload: []byte(source),
							Metadata: map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{
								ID: "synthetic-route", Type: provider, SupportConfigurationUpdate: supported,
								Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
							}},
						}
						before := bytes.Clone(request.Payload)
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
						var body []byte
						if provider == "codex" {
							prepared, err := codexResponsesBody(ctx, &config.Config{}, nil, request.Model, request.Model, provider, opts.SourceFormat, opts, request, streaming, false)
							if err != nil {
								t.Fatal(err)
							}
							body = prepared.Body
						} else {
							prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequestWithContext(ctx, nil, request, opts, streaming, sdktranslator.FormatCodex)
							if err != nil {
								t.Fatal(err)
							}
							body = prepared.body
						}
						wantTop := "medium"
						if !supported && effort != "" {
							wantTop = effort
						}
						if got := gjson.GetBytes(body, "reasoning.effort").String(); got != wantTop {
							t.Fatalf("top-level effort = %q, want %q; body=%s", got, wantTop, body)
						}
						wantTurn := "medium"
						if effort != "" {
							wantTurn = effort
						}
						if got := thinking.ExtractTranslatedReasoningEffort(body, provider); got != wantTurn {
							t.Fatalf("effective effort = %q, want %q; body=%s", got, wantTurn, body)
						}
						wantUpdates := supported && effort != ""
						if got := gjson.GetBytes(body, "input.0.type").String() == "configuration_update"; got != wantUpdates {
							t.Fatalf("update present = %v, want %v; body=%s", got, wantUpdates, body)
						}
						if hooks.requestCalls.Load() != 1 || !bytes.Equal(request.Payload, before) || len(request.Metadata) != 1 {
							t.Fatalf("request scope changed: calls=%d metadata=%v", hooks.requestCalls.Load(), request.Metadata)
						}
					})
				}
			}
		}
	}
}

type claudeThinkingNormalizerHooks struct {
	executorTranslatorHooks
	effort    string
	sawClaude bool
}

func (h *claudeThinkingNormalizerHooks) NormalizeRequest(_ context.Context, _, to sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.requestCalls.Add(1)
	h.sawClaude = to == sdktranslator.FormatClaude && gjson.GetBytes(body, "messages").IsArray()
	if !h.sawClaude {
		return body
	}
	if h.effort == "visibility" {
		out, _ := sjson.SetBytes(body, "thinking.display", "summarized")
		return out
	}
	if h.effort == "" {
		out, _ := sjson.DeleteBytes(body, "thinking")
		out, _ = sjson.DeleteBytes(out, "output_config.effort")
		return out
	}
	out, _ := sjson.SetBytes(body, "thinking.type", "adaptive")
	out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
	out, _ = sjson.SetBytes(out, "output_config.effort", h.effort)
	return out
}

func TestClaudePreparationPreservesThinkingNormalizerIntent(t *testing.T) {
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hi"}]}`
	for _, streaming := range []bool{false, true} {
		for _, effort := range []string{"", "high", "visibility"} {
			t.Run(fmt.Sprintf("stream=%v/edit=%s", streaming, effort), func(t *testing.T) {
				hooks := &claudeThinkingNormalizerHooks{effort: effort}
				ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
				request := cliproxyexecutor.Request{
					Model: "synthetic-claude-normalizer", Payload: []byte(source),
					Metadata: map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{
						ID: "synthetic-claude-normalizer", Type: "claude",
						Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
					}},
				}
				before := bytes.Clone(request.Payload)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				prepared, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(ctx, nil, request, opts, streaming)
				if err != nil {
					t.Fatal(err)
				}
				var payload bytes.Buffer
				if _, err := payload.ReadFrom(prepared.httpReq.Body); err != nil {
					t.Fatal(err)
				}
				if err := prepared.httpReq.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if !hooks.sawClaude || hooks.requestCalls.Load() != 1 || !bytes.Equal(request.Payload, before) || len(request.Metadata) != 1 {
					t.Fatal("Claude normalization did not receive the target schema or changed the caller request")
				}
				if effort == "visibility" {
					if gjson.GetBytes(payload.Bytes(), "output_config.effort").String() != "low" ||
						gjson.GetBytes(payload.Bytes(), "thinking.display").String() != "summarized" {
						t.Fatal("summary visibility normalization replaced the current configuration update")
					}
					return
				}
				if effort == "" {
					if gjson.GetBytes(payload.Bytes(), "thinking").Exists() || gjson.GetBytes(payload.Bytes(), "output_config.effort").Exists() {
						t.Fatal("Claude preparation restored thinking removed by the request normalizer")
					}
				} else if got := gjson.GetBytes(payload.Bytes(), "output_config.effort").String(); got != effort {
					t.Fatalf("prepared Claude effort = %s, want %s from target request normalizer", got, effort)
				}
			})
		}
	}
}
