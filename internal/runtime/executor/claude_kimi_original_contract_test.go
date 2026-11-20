package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func TestKimiClaudeResponsePreservesOriginalLogicalModel(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai"} {
		for _, model := range []string{"k3", "kimi-k3", "kimi-k2.6"} {
			for _, operation := range []string{"messages", "stream"} {
				for _, format := range []translator.Format{translator.FormatClaude, translator.FormatOpenAI} {
					t.Run(
						provider+"/"+model+"/"+operation+"/"+format.String(), func(t *testing.T) {
							var upstream []byte
							ctx := util.WithRoundTripper(
								t.Context(), claudeContractTransport(
									func(request *http.Request) (*http.Response, error) {
										var err error
										upstream, err = io.ReadAll(request.Body)
										if err != nil {
											return nil, err
										}
										response := claudeContractResponse(request, upstream)
										data, err := io.ReadAll(response.Body)
										if err != nil {
											return nil, err
										}
										if err := response.Body.Close(); err != nil {
											return nil, err
										}
										data = bytes.ReplaceAll(
											data, []byte(`"model":"claude-opus-5"`),
											[]byte(fmt.Sprintf(
												`"model":%q`, gjson.GetBytes(upstream, "model").String(),
											)),
										)
										response.Body = io.NopCloser(bytes.NewReader(data))
										return response, nil
									},
								),
							)
							execute := NewKimiExecutor(&config.Config{})
							if provider == "kimi-ai" {
								execute = NewKimiAIExecutor(&config.Config{})
							}
							credential := &auth.Auth{
								ID: t.Name(), Provider: provider,
								Attributes: map[string]string{
									"api_key": "synthetic-key", "base_url": "https://kimi.invalid",
								},
							}
							payload := []byte(fmt.Sprintf(
								`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`,
								model,
							))
							before := bytes.Clone(payload)
							request := core.Request{Model: model, Payload: payload}
							opts := core.Options{
								SourceFormat: translator.FormatClaude, ResponseFormat: format, OriginalRequest: payload,
							}
							var output []byte
							if operation == "messages" {
								response, err := execute.Execute(ctx, credential, request, opts)
								if err != nil {
									t.Fatal(err)
								}
								output = response.Payload
							} else {
								stream, err := execute.ExecuteStream(ctx, credential, request, opts)
								if err != nil {
									t.Fatal(err)
								}
								for chunk := range stream.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
									output = append(output, chunk.Payload...)
								}
							}
							if got := gjson.GetBytes(
								upstream, "model",
							).String(); got != normalizeKimiUpstreamModel(model) {
								t.Fatalf("upstream model=%q, request=%s", got, upstream)
							}
							responseModel := gjson.GetBytes(output, "model").String()
							if operation == "stream" {
								for line := range bytes.SplitSeq(output, []byte("\n")) {
									root := gjson.ParseBytes(jsonPayload(line))
									path := "model"
									if format == translator.FormatClaude {
										path = "message.model"
									}
									if value := root.Get(path).String(); value != "" {
										responseModel = value
									}
								}
							}
							if responseModel != model {
								t.Fatalf(
									"response model=%q, want logical model=%q, payload=%s",
									responseModel,
									model,
									output,
								)
							}
							if request.Model != model || !bytes.Equal(request.Payload, before) {
								t.Fatal("caller request changed during delegated execution")
							}
						},
					)
				}
			}
		}
	}
}

func TestClaudeSignatureSanitizeKeepsSafeRequestDiagnostics(t *testing.T) {
	for _, mode := range []string{"changed", "unchanged", "info-level"} {
		t.Run(
			mode, func(t *testing.T) {
				logger := log.StandardLogger()
				oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.GetLevel()
				var output bytes.Buffer
				logger.SetOutput(&output)
				logger.SetFormatter(&log.JSONFormatter{DisableTimestamp: true})
				logger.SetLevel(log.DebugLevel)
				if mode == "info-level" {
					logger.SetLevel(log.InfoLevel)
				}
				t.Cleanup(
					func() {
						logger.SetOutput(oldOutput)
						logger.SetFormatter(oldFormatter)
						logger.SetLevel(oldLevel)
					},
				)
				messages := `[{"role":"user","content":"synthetic request"}]`
				if mode != "unchanged" {
					messages = `[{"role":"assistant","content":[` +
						(`{"type":"thinking","thinking":"private reasoning text","signature":` +
							`"synthetic-private-signature"},`) +
						`{"type":"text","text":"retained content"},` +
						(`{"type":"tool_use","id":"call_1","name":"lookup","input":{},` +
							`"thoughtSignature":"synthetic-private-tool-signature"}`) +
						(`]},{"role":"user","content":[{"type":"tool_result","tool_use_id":` +
							`"call_1","content":"synthetic ` +
							`result"}]}]`)
				}
				ctx := logging.WithRequestID(t.Context(), "synthetic-request-id")
				credential := &auth.Auth{
					ID: t.Name(), Provider: "claude",
					Attributes: map[string]string{
						"api_key": "synthetic-private-token", "base_url": "https://claude.invalid",
						"cloak_mode": "never",
					},
				}
				payload := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":` + messages + `}`)
				prepared, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(
					ctx, credential,
					core.Request{Model: "claude-sonnet-5", Payload: payload},
					core.Options{SourceFormat: translator.FormatClaude}, false,
				)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "unchanged" && (bytes.Contains(
					prepared.bodyForTranslation, []byte("synthetic-private-signature"),
				) ||
					bytes.Contains(prepared.bodyForTranslation, []byte("synthetic-private-tool-signature"))) {
					t.Fatalf("incompatible signatures remain in upstream body: %s", prepared.bodyForTranslation)
				}
				var entries []map[string]any
				for line := range bytes.SplitSeq(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
					if gjson.GetBytes(line, "component").String() != "signature_sanitizer" {
						continue
					}
					var entry map[string]any
					if err := json.Unmarshal(line, &entry); err != nil {
						t.Fatal(err)
					}
					entries = append(entries, entry)
				}
				if mode != "changed" {
					if len(entries) != 0 {
						t.Fatalf("unexpected diagnostics: %s", output.Bytes())
					}
					return
				}
				if len(entries) != 1 {
					t.Fatalf("want one request summary, got %d: %s", len(entries), output.Bytes())
				}
				entry := entries[0]
				for key, value := range map[string]any{
					"request_id": "synthetic-request-id", "executor": "claude", "action": "sanitize_claude_messages",
					"target_provider": "claude", "target_model": "claude-sonnet-5",
					"dropped_blocks": float64(1), "dropped_signatures": float64(1), "replaced_signatures": float64(0),
					"first_block_kind": "claude_thinking", "first_detected_provider": "unknown",
				} {
					if entry[key] != value {
						t.Fatalf("%s=%v, want %v: %s", key, entry[key], value, output.Bytes())
					}
				}
				if entry["first_reason"] == nil || entry["first_reason"] == "" {
					t.Fatalf("missing cleanup reason: %s", output.Bytes())
				}
				for _, private := range []string{
					"synthetic-private-signature", "synthetic-private-tool-signature", "private reasoning text",
					"retained content", "synthetic-private-token",
				} {
					if strings.Contains(output.String(), private) {
						t.Fatalf("private request content appeared in diagnostics: %q", private)
					}
				}
			},
		)
	}
}
