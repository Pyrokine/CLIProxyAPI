package executor

import (
	"bytes"
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
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestAuditClaudeExplicitLegacyCloakSettingsReachTransport(t *testing.T) {
	for _, source := range []string{"attributes", "metadata"} {
		for _, field := range []string{"cloak_strict_mode", "cloak_sensitive_words", "cloak_cache_user_id"} {
			for _, mode := range []string{"auto", "never"} {
				for _, operation := range []string{"messages", "stream"} {
					t.Run(
						source+"/"+field+"/"+mode+"/"+operation, func(t *testing.T) {
							credential := &auth.Auth{
								ID: t.Name(), Provider: "claude", Attributes: map[string]string{
									"api_key": "synthetic-api-" + t.Name(), "base_url": "https://api.anthropic.com",
								}, Metadata: make(map[string]any),
							}
							value := "true"
							if field == "cloak_sensitive_words" {
								//goland:noinspection SpellCheckingInspection
								value = "synthetic-secretword"
							}
							if source == "attributes" {
								credential.Attributes[field] = value
							} else {
								credential.Metadata[field] = value
							}
							if mode == "never" {
								credential.Attributes["cloak_mode"] = "never"
							}
							var sent []byte
							ctx := util.WithRoundTripper(
								t.Context(), claudeContractTransport(
									func(request *http.Request) (*http.Response, error) {
										var err error
										sent, err = io.ReadAll(request.Body)
										if err != nil {
											return nil, err
										}
										return claudeContractResponse(request, sent), nil
									},
								),
							)
							execute := NewClaudeExecutor(&config.Config{})
							//goland:noinspection SpellCheckingInspection
							request := core.Request{
								Model: "claude-sonnet-5",
								Payload: []byte(
									`{"model":"claude-sonnet-5","max_tokens":100,"system":"caller-guidance"` +
										`,"messages":[{"role":"user","content":"Review the synthetic-secretword` +
										` build result carefully"}]}`,
								),
							}
							opts := core.Options{SourceFormat: translator.FormatClaude}
							cachedID := ""
							for range 2 {
								executeClaudeContractRequest(t, execute, ctx, credential, request, opts, operation)
								cloaked := bytes.Contains(sent, []byte("x-anthropic-billing-header:"))
								if cloaked != (mode == "auto") {
									t.Fatalf("cloaked=%t body=%s", cloaked, sent)
								}
								if field == "cloak_strict_mode" && bytes.Contains(
									sent, []byte("caller-guidance"),
								) != (mode == "never") {
									t.Fatalf("strict system handling=%s", sent)
								}
								//goland:noinspection SpellCheckingInspection
								if field == "cloak_sensitive_words" && bytes.Contains(
									sent, []byte("synthetic-secretword"),
								) != (mode == "never") {
									t.Fatalf("sensitive word handling=%s", sent)
								}
								if field == "cloak_cache_user_id" {
									id := gjson.GetBytes(sent, "metadata.user_id").String()
									if mode == "never" && id != "" {
										t.Fatalf("never added identity=%s", sent)
									}
									if mode == "auto" && (id == "" || (cachedID != "" && id != cachedID)) {
										t.Fatalf("cached ID=%q previous=%q", id, cachedID)
									}
									cachedID = id
								}
							}
						},
					)
				}
			}
		}
	}
}

func TestAuditClaudeBillingSurvivesFinalRulesAndSensitiveWords(t *testing.T) {
	for _, system := range []string{"control", "string", "array"} {
		for _, sensitive := range []bool{false, true} {
			for _, subagent := range []bool{false, true} {
				for _, operation := range []string{"messages", "stream"} {
					t.Run(
						fmt.Sprintf("%s/sensitive=%t/subagent=%t/%s", system, sensitive, subagent, operation),
						func(t *testing.T) {
							cfg := &config.Config{}
							if system != "control" {
								var value any = "Operator-owned guidance"
								if system == "array" {
									value = []any{map[string]any{"type": "text", "text": "Operator-owned guidance"}}
								}
								cfg.Payload.Override = []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{
												Name: "claude-sonnet-5", Protocol: "claude",
											},
										}, Params: map[string]any{"system": value},
									},
								}
							}
							//goland:noinspection SpellCheckingInspection
							credential := &auth.Auth{
								ID: t.Name(), Provider: "claude", Attributes: map[string]string{
									"api_key": "sk-ant-oat-synthetic-wire", "base_url": "https://api.anthropic.com",
									"fingerprint_profile": "claude-code-cli", "cloak_mode": "always",
								}, Metadata: map[string]any{
									"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
									"claude_device_ids": []string{strings.Repeat("0", 64)},
								},
							}
							if sensitive {
								credential.Attributes["cloak_sensitive_words"] = "cc_version,cc_entrypoint"
							}
							opts := core.Options{
								SourceFormat: translator.FormatClaude,
								Metadata:     map[string]any{core.ExecutionSessionMetadataKey: uuid.NewString()},
							}
							if subagent {
								opts.Headers = http.Header{"X-Claude-Code-Agent-Id": {"synthetic-agent"}}
							}
							var sent []byte
							call := 0
							ctx := util.WithRoundTripper(
								t.Context(), claudeContractTransport(
									func(request *http.Request) (*http.Response, error) {
										var err error
										sent, err = io.ReadAll(request.Body)
										if err != nil {
											return nil, err
										}
										call++
										response := claudeContractResponse(request, sent)
										response.Header.Set("request-id", fmt.Sprintf("req_synthetic_turn_%d", call))
										return response, nil
									},
								),
							)
							execute := NewClaudeExecutor(cfg)
							for turn := 1; turn <= 2; turn++ {
								tail := ""
								if turn == 2 {
									tail = `,{"role":"assistant","content":"synthetic response"},{"role":"` +
										`user","content":"Continue reviewing the synthetic build result ` +
										`carefully"}`
								}
								payload := []byte(
									(`{"model":"claude-sonnet-5","max_tokens":100,"system":"caller cc_version ` +
										`cc_entrypoint guidance","messages":[{"role":"user","content":"` +
										`Review cc_version and cc_entrypoint for synthetic turn 1 carefully"}`) +
										tail + `]}`,
								)
								executeClaudeContractRequest(
									t, execute, ctx, credential,
									core.Request{Model: "claude-sonnet-5", Payload: payload}, opts, operation,
								)
								billing := gjson.GetBytes(sent, "system.0.text").String()
								if !strings.HasPrefix(
									billing, "x-anthropic-billing-header:",
								) || !strings.Contains(billing, "cc_version=") || !strings.Contains(
									billing, "cc_entrypoint=cli;",
								) || !strings.Contains(billing, "cch=") {
									t.Fatalf("billing fields=%q body=%s", billing, sent)
								}
								if strings.Contains(billing, "cc_is_subagent=true;") != subagent || extractTag(
									billing, "cc_prompt_id=",
								) == "" {
									t.Fatalf("billing identity=%q", billing)
								}
								if turn == 2 && !strings.Contains(billing, "cc_prev_req=req_synthetic_turn_1;") {
									t.Fatalf("second-turn continuity=%q", billing)
								}
								if system != "control" && gjson.GetBytes(
									sent, "system.1.text",
								).String() != "Operator-owned guidance" {
									t.Fatalf("operator guidance changed: %s", sent)
								}
								if sensitive && (strings.Contains(
									gjson.GetBytes(sent, "messages").Raw, "cc_version",
								) || strings.Contains(gjson.GetBytes(sent, "messages").Raw, "cc_entrypoint")) {
									t.Fatalf("caller words remained unobfuscated: %s", sent)
								}
								signed, err := signAnthropicMessagesBody(sent)
								if err != nil || !bytes.Equal(signed, sent) {
									t.Fatalf("final CCH mismatch: %v body=%s", err, sent)
								}
							}
						},
					)
				}
			}
		}
	}
}
