package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeChatToolConstraintsAndCacheReachTransport(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, test := range []struct{ name, choice, wantType string }{
			{"none", `"none"`, "none"},
			{"specific", `{"type":"function","function":{"name":"two"}}`, "tool"},
			{"allowed", `{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"two"}}]}}`, "any"},
		} {
			name := "api-key/"
			if oauth {
				name = "oauth/"
			}
			t.Run(name+test.name, func(t *testing.T) {
				var bodies [][]byte
				ctx := util.WithRoundTripper(context.Background(), kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					bodies = append(bodies, body)
					toolName := gjson.GetBytes(body, "tools.0.name").String()
					if test.wantType == "tool" {
						toolName = gjson.GetBytes(body, "tool_choice.name").String()
					}
					block := `{"type":"text","text":"fixture"}`
					stop := "end_turn"
					if test.wantType != "none" {
						encodedName, _ := json.Marshal(toolName)
						block = `{"type":"tool_use","id":"toolu_fixture","name":` + string(encodedName) + `,"input":{}}`
						stop = "tool_use"
					}
					sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
						"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":" + block + "}\n\n" +
						"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
						"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop + "\"},\"usage\":{\"output_tokens\":1}}\n\n" +
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
				}))
				auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fixture-key"}}
				if oauth {
					auth.Attributes["api_key"] = "sk-ant-oat01-fixture"
					auth.Metadata = map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "claude_device_ids": []string{strings.Repeat("a", 64)}}
				}
				executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
				payload := `{"model":"claude-sonnet-4-5","prompt_cache_options":{"mode":"explicit"},"parallel_tool_calls":false,"tool_choice":` + test.choice + `,"tools":[{"type":"function","function":{"name":"one","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"two","parameters":{"type":"object","properties":{}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"cached user","cache_control":{"type":"ephemeral","ttl":"5m"}}]},{"role":"assistant","tool_calls":[{"id":"call_previous","type":"function","function":{"name":"two","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_previous","content":[{"type":"text","text":"result","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`
				response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(payload)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
				if err != nil {
					t.Fatal(err)
				}
				body := bodies[0]
				if gjson.GetBytes(body, "tool_choice.type").String() != test.wantType ||
					gjson.GetBytes(body, "tool_choice.disable_parallel_tool_use").Exists() != (test.wantType != "none") {
					t.Fatalf("transport tool constraints changed: %s", body)
				}
				if test.name == "allowed" && gjson.GetBytes(body, "tools.#").Int() != 1 {
					t.Fatalf("disallowed declaration reached transport: %s", body)
				}
				cachedUser, cachedResult := false, false
				for _, message := range gjson.GetBytes(body, "messages").Array() {
					for _, block := range message.Get("content").Array() {
						if block.Get("type").String() == "text" && block.Get("text").String() == "cached user" {
							cachedUser = block.Get("cache_control.type").String() == "ephemeral"
						}
						if block.Get("type").String() == "tool_result" {
							cachedResult = block.Get("cache_control.type").String() == "ephemeral"
							for _, part := range block.Get("content").Array() {
								if part.Get("cache_control").Exists() {
									t.Fatalf("nested cache marker reached transport: %s", body)
								}
							}
						}
					}
				}
				if !cachedUser || !cachedResult {
					t.Fatalf("explicit cache placement lost: %s", body)
				}
				if test.wantType == "none" {
					return
				}
				calls := gjson.GetBytes(response.Payload, "choices.0.message.tool_calls")
				if calls.Get("0.function.name").String() != "two" || calls.Get("0.id").String() == "" {
					t.Fatalf("caller tool identity not restored: %s", response.Payload)
				}
				replay := `{"model":"claude-sonnet-4-5","tools":[{"type":"function","function":{"name":"two"}}],"messages":[{"role":"assistant","tool_calls":` + calls.Raw + `},{"role":"tool","tool_call_id":"` + calls.Get("0.id").String() + `","content":"fixture result"}]}`
				if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(replay)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestClaudeExplicitPromptCacheModeInCountTokens(t *testing.T) {
	for _, tc := range []struct {
		name, cacheControl string
	}{
		{name: "no caller marker"},
		{name: "caller 1h marker", cacheControl: `,"cache_control":{"type":"ephemeral","ttl":"1h"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentBodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v1/messages/count_tokens" {
					http.Error(w, "unexpected path", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				sentBodies <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"input_tokens":12}`)
			}))
			defer server.Close()

			request := `{"model":"claude-opus-4-7","prompt_cache_options":{"mode":" ExPlIcIt "},"messages":[{"role":"user","content":[{"type":"text","text":"hello"` + tc.cacheControl + `}]}]}`
			auth := &cliproxyauth.Auth{
				Provider: "claude",
				Attributes: map[string]string{
					"api_key": "sk-ant-oat01-test", "base_url": server.URL, "cloak_mode": "always",
				},
				Metadata: map[string]any{
					"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
					"claude_device_ids": []string{strings.Repeat("a", 64)},
				},
			}
			_, err := NewClaudeExecutor(&config.Config{}).countTokensUpstream(context.Background(), auth,
				cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: []byte(request)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if err != nil {
				t.Fatal(err)
			}
			sentBody := <-sentBodies
			if gjson.GetBytes(sentBody, "prompt_cache_options").Exists() {
				t.Fatalf("prompt cache options forwarded upstream: %s", sentBody)
			}
			count := countCacheControls(sentBody)
			if tc.cacheControl == "" && count != 0 {
				t.Fatalf("explicit mode inserted %d cache markers: %s", count, sentBody)
			}
			if tc.cacheControl != "" && (count != 1 ||
				gjson.GetBytes(sentBody, "messages.0.content.0.cache_control.ttl").String() != "1h") {
				t.Fatalf("caller cache marker changed: %s", sentBody)
			}
		})
	}
}

func TestClaudeExplicitPromptCacheModePreservesCallerPlacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cloaked bool
		marker  bool
	}{
		{name: "native request"},
		{name: "cloaked request", cloaked: true},
		{name: "caller marker", marker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{DisableClaudeCloakMode: !tc.cloaked}
			request := `{"model":"claude-opus-4-7","prompt_cache_options":{"mode":" ExPlIcIt "},"system":"Use plain language","messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}],"max_tokens":16}`
			if tc.marker {
				request = `{"model":"claude-opus-4-7","prompt_cache_options":{"mode":"explicit"},"messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"max_tokens":16}`
			}
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fixture-api-key", "base_url": "https://example.invalid"}}
			if tc.cloaked {
				auth.Attributes["cloak_mode"] = "always"
			}
			prepared, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: []byte(request)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(prepared.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(body, "prompt_cache_options").Exists() {
				t.Fatalf("prompt cache options forwarded upstream: %s", body)
			}
			if tc.marker {
				if got := gjson.GetBytes(body, "messages.0.content.0.cache_control.ttl").String(); got != "1h" || countCacheControls(body) != 1 {
					t.Fatalf("caller cache marker changed: %s", body)
				}
			} else if strings.Contains(string(body), `"cache_control"`) {
				t.Fatalf("explicit cache mode injected breakpoints: %s", body)
			}
		})
	}
}
