package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const fingerprintPayload = `{"model":"claude-opus-4-7","system":"caller system","max_tokens":100,"messages":[{"role":"user","content":"first prompt for a synthetic request"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`

func fingerprintAuth(profile, base string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "synthetic-profile", Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-api-key", "base_url": base, "fingerprint_profile": profile}}
}
func fingerprintOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "synthetic-conversation"}, Headers: http.Header{"User-Agent": {"caller/1"}, "Anthropic-Beta": {"future-caller-beta"}}}
}
func fingerprintRequest(payload string) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: []byte(payload)}
}

func TestClaudeFingerprintWirePolicy(t *testing.T) {
	for _, test := range []struct {
		name, profile, base string
		cli, sign           bool
	}{
		{"default direct", "", "https://api.anthropic.com", false, false},
		{"CLI direct", "claude-code-cli", "https://api.anthropic.com", true, true},
		{"legacy profile", " OAUTH-CLI ", "https://api.anthropic.com", true, true},
		{"CLI gateway", "claude-code-cli", "https://gateway.invalid", true, false},
		{"CLI insecure", "claude-code-cli", "http://api.anthropic.com", true, false},
		{"CLI other port", "claude-code-cli", "https://api.anthropic.com:8443", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := fingerprintAuth(test.profile, test.base)
			exec := NewClaudeExecutor(&config.Config{})
			cr, err := exec.prepareClaudeRequest(context.Background(), auth, fingerprintRequest(fingerprintPayload), fingerprintOptions(), false)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(cr.httpReq.Body)
			if got := strings.HasPrefix(cr.httpReq.Header.Get("User-Agent"), "claude-cli/2.1.258"); got != test.cli {
				t.Fatalf("UA=%q", cr.httpReq.Header.Get("User-Agent"))
			}
			if got := strings.Contains(gjson.GetBytes(body, "system.0.text").String(), "cch="); got != test.sign {
				t.Fatalf("billing=%s", gjson.GetBytes(body, "system").Raw)
			}
			if !strings.Contains(cr.httpReq.Header.Get("Anthropic-Beta"), "future-caller-beta") {
				t.Fatal("caller beta lost")
			}
			if test.cli {
				if cr.httpReq.Header.Get("X-Stainless-Package-Version") != "0.112.1" || cr.httpReq.Header.Get("X-Stainless-Runtime-Version") != "v26.3.0" {
					t.Fatal("baseline mismatch")
				}
				identity := gjson.Parse(gjson.GetBytes(body, "metadata.user_id").String())
				if identity.Get("session_id").String() != cr.httpReq.Header.Get("X-Claude-Code-Session-Id") || identity.Get("account_uuid").String() == "" {
					t.Fatal("body/header identity mismatch")
				}
				if !strings.HasPrefix(gjson.GetBytes(body, "tools.0.name").String(), "mcp__") {
					t.Fatal("CLI tool was not aliased")
				}
				if !strings.Contains(cr.httpReq.Header.Get("Anthropic-Beta"), claudeOAuthBeta) {
					t.Fatal("CLI beta absent")
				}
			} else {
				if cr.httpReq.Header.Get("X-App") != "" ||
					gjson.GetBytes(body, "metadata.user_id").Exists() ||
					gjson.GetBytes(body, "system.0.text").String() != "caller system" {
					t.Fatalf("caller-owned identity mutated: %s", body)
				}
			}
			if auth.Metadata != nil {
				t.Fatal("shared metadata changed")
			}
			if helps.IsAnthropicUpstreamURL(cr.httpReq.URL) && cr.httpReq.Header.Get("X-Api-Key") != "synthetic-api-key" {
				t.Fatal("CLI profile changed API-key authentication")
			}
		})
	}
}

func TestClaudeFingerprintPresencePriority(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-api-key", FingerprintProfile: "claude-code-cli"}}}
	a := fingerprintAuth("", "")
	a.Metadata = map[string]any{"fingerprint_profile": "claude-code-cli", "fingerprint-profile": "claude-code-cli"}
	if resolveClaudeFingerprintPolicy(cfg, a, "synthetic-api-key").ProfileClaudeCodeCLI {
		t.Fatal("explicit empty attributes lost")
	}
	delete(a.Attributes, "fingerprint_profile")
	a.Metadata["fingerprint_profile"] = ""
	if resolveClaudeFingerprintPolicy(cfg, a, "synthetic-api-key").ProfileClaudeCodeCLI {
		t.Fatal("empty canonical metadata lost")
	}
	delete(a.Metadata, "fingerprint_profile")
	a.Metadata["fingerprint-profile"] = ""
	if resolveClaudeFingerprintPolicy(cfg, a, "synthetic-api-key").ProfileClaudeCodeCLI {
		t.Fatal("empty legacy metadata lost")
	}
	delete(a.Metadata, "fingerprint-profile")
	if !resolveClaudeFingerprintPolicy(cfg, a, "synthetic-api-key").ProfileClaudeCodeCLI {
		t.Fatal("missing profile ignored config")
	}
}

func fingerprintResponse(name string, stream bool) string {
	if !stream {
		return fmt.Sprintf(`{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"tool_use","id":"tool_synthetic","name":%q,"input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":1}}`, name)
	}
	return fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_synthetic\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-7\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_synthetic\",\"name\":%q,\"input\":{}}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n", name)
}

func TestClaudeFingerprintMCPAllResponsePaths(t *testing.T) {
	for _, mode := range []string{"json", "aggregate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(r.Body)
				name := gjson.GetBytes(body, "tools.0.name").String()
				if name == "lookup" {
					t.Fatal("tool not aliased")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Request-Id": {"req_synthetic"}}, Body: io.NopCloser(strings.NewReader(fingerprintResponse(name, mode != "json"))), Request: r}, nil
			}))
			a := fingerprintAuth("claude-code-cli", "https://api.anthropic.com")
			opts := fingerprintOptions()
			if mode == "aggregate" {
				opts.ResponseFormat = sdktranslator.FormatOpenAI
			}
			e := NewClaudeExecutor(&config.Config{})
			var payload []byte
			if mode == "stream" {
				result, err := e.ExecuteStream(ctx, a, fingerprintRequest(fingerprintPayload), opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
			} else {
				result, err := e.Execute(ctx, a, fingerprintRequest(fingerprintPayload), opts)
				if err != nil {
					t.Fatal(err)
				}
				payload = result.Payload
			}
			if !bytes.Contains(payload, []byte(`"name":"lookup"`)) || bytes.Contains(payload, []byte("mcp__")) {
				t.Fatalf("tool not restored: %s", payload)
			}
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
		})
	}
}

func TestClaudeFingerprintMCPPassthroughConflict(t *testing.T) {
	alias := helps.ClaudeMCPToolAlias("conflict", "lookup", 0)
	parts, _ := parseClaudeMCPAlias(alias)
	original := "mcp__" + parts.server + "__caller_tool"
	body := []byte(fmt.Sprintf(`{"tools":[{"name":"lookup"},{"name":%q}]}`, original))
	_, reverse := prepareClaudeOAuthToolNamesForUpstream(body, claudeMCPAliasOptions{callerIdentity: "conflict"})
	response := []byte(fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q}]}`, original))
	out, err := restoreClaudeOAuthToolNamesFromResponse(response, reverse)
	if err != nil || !bytes.Equal(out, response) {
		t.Fatalf("passthrough=%s err=%v", out, err)
	}
}

func TestClaudeFingerprintCountTokensRealEntrypoints(t *testing.T) {
	for _, cli := range []bool{false, true} {
		for _, kimi := range []bool{false, true} {
			t.Run(fmt.Sprintf("cli=%v/kimi=%v", cli, kimi), func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if !strings.Contains(r.URL.Path, "count_tokens") {
						t.Fatalf("control request: %s", r.URL)
					}
					body, _ := io.ReadAll(r.Body)
					if bytes.Contains(body, []byte("x-anthropic-billing-header")) {
						t.Fatal("generation billing in count request")
					}
					wantFields := kimi && !cli
					for _, field := range []string{"metadata", "context_management", "diagnostics"} {
						if gjson.GetBytes(body, field).Exists() != wantFields {
							t.Fatalf("%s presence=%v body=%s", field, !wantFields, body)
						}
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"input_tokens":17}`)), Request: r}, nil
				}))
				profile := ""
				if cli {
					profile = "claude-code-cli"
				}
				a := fingerprintAuth(profile, "")
				payload := `{"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"caller"},"context_management":{},"diagnostics":{}}`
				var response cliproxyexecutor.Response
				var err error
				if kimi {
					a.Provider = "kimi"
					delete(a.Attributes, "api_key")
					a.Metadata = map[string]any{"access_token": "synthetic-kimi"}
					response, err = NewKimiExecutor(&config.Config{}).CountTokens(ctx, a, fingerprintRequest(payload), fingerprintOptions())
				} else {
					response, err = NewClaudeExecutor(&config.Config{}).CountTokens(ctx, a, fingerprintRequest(payload), fingerprintOptions())
				}
				if err != nil || calls != 1 || gjson.GetBytes(response.Payload, "input_tokens").Int() != 17 {
					t.Fatalf("calls=%d response=%s err=%v", calls, response.Payload, err)
				}
				if a.Attributes["base_url"] != "" {
					t.Fatal("Kimi count changed shared base URL")
				}
			})
		}
	}
}

func TestClaudeFingerprintKimiWireAndAttribution(t *testing.T) {
	e := NewKimiExecutor(&config.Config{})
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatCodex} {
		want := sdktranslator.FormatOpenAI
		if format == sdktranslator.FormatClaude {
			want = format
		}
		if got := e.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{SourceFormat: format}); got != want {
			t.Fatalf("format=%s want=%s", got, want)
		}
	}
	a := fingerprintAuth("", "https://api.kimi.com")
	a.Provider = "kimi"
	delete(a.Attributes, "api_key")
	a.Metadata = map[string]any{"access_token": "synthetic-kimi"}
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cch=abcde;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"caller rule"}],"messages":[{"role":"user","content":"hello"}]}`
	cr, err := e.ClaudeExecutor.prepareClaudeRequest(context.Background(), a, fingerprintRequest(body), fingerprintOptions(), false)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(cr.httpReq.Body)
	if bytes.Contains(out, []byte("billing-header")) ||
		!bytes.Contains(out, []byte(claudeCodeCLIIdentity)) ||
		!bytes.Contains(out, []byte("caller rule")) {
		t.Fatalf("attribution=%s", out)
	}
	if e.ClaudeExecutor.ShouldPrepareRequestAuth(a) {
		t.Fatal("delegated credential requests OAuth profile")
	}
	if cr.httpReq.Header.Get("Authorization") != "Bearer synthetic-kimi" || cr.httpReq.Header.Get("X-App") != "" {
		t.Fatal("delegated default auth/profile incorrect")
	}
}

func TestClaudeFingerprintContinuityFinalBodyAndCompletion(t *testing.T) {
	helps.ResetClaudeDiagnosticsForTest()
	a := fingerprintAuth("claude-code-cli", "https://api.anthropic.com")
	e := NewClaudeExecutor(&config.Config{})
	opts := fingerprintOptions()
	makeRequest := func(payload string) *claudeRequest {
		cr, err := e.prepareClaudeRequest(context.Background(), a, fingerprintRequest(payload), opts, false)
		if err != nil {
			t.Fatal(err)
		}
		return cr
	}
	first := makeRequest(fingerprintPayload)
	firstBody, _ := io.ReadAll(first.httpReq.Body)
	_, firstPrompt := helps.ExtractClaudeBillingTags(firstBody)
	if firstPrompt == "" {
		t.Fatal("prompt tag absent")
	}
	first.diagnosticsState.requestID = "req_first"
	commitClaudeDiagnostics(first.diagnosticsState, "msg_first")
	secondPayload, _ := sjson.SetRawBytes([]byte(fingerprintPayload), "messages.-1", []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"continued"}]}`))
	second := makeRequest(string(secondPayload))
	body, _ := io.ReadAll(second.httpReq.Body)
	prev, prompt := helps.ExtractClaudeBillingTags(body)
	if prev != "req_first" || prompt != firstPrompt || gjson.GetBytes(body, "diagnostics.previous_message_id").String() != "msg_first" {
		t.Fatalf("continuity=%s", body)
	}
	probe := makeRequest(`{"max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`)
	probeBody, _ := io.ReadAll(probe.httpReq.Body)
	if probe.diagnosticsState.key != "" || gjson.GetBytes(probeBody, "diagnostics").Exists() {
		t.Fatal("probe advanced continuity")
	}
	commitClaudeDiagnostics(probe.diagnosticsState, "msg_probe")
	observer := newClaudeDiagnosticsStreamObserver(second.diagnosticsState)
	observer.ObserveClaudeSSELine([]byte(`data: {"type":"message_start","message":{"id":"msg_canceled"}}`))
	observer.ObserveClaudeSSELine([]byte(`data: {"type":"message_stop"}`))
	observer.FinishClaudeStream(claudeStreamOutcomeCanceled)
	third := makeRequest(string(secondPayload))
	thirdBody, _ := io.ReadAll(third.httpReq.Body)
	if gjson.GetBytes(thirdBody, "diagnostics.previous_message_id").String() != "msg_first" {
		t.Fatal("canceled response committed")
	}
	if claudeBillingFingerprintMessageText([]byte(fingerprintPayload)) != claudeBillingFingerprintMessageText(secondPayload) {
		t.Fatal("billing fingerprint moved to latest turn")
	}
}

func TestClaudeFingerprintCancellationAuthority(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := newClaudeOAuthCancellationError(ctx, oauth, context.Canceled)
		if (err != nil) != oauth {
			t.Fatalf("oauth=%v err=%v", oauth, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	consumeClaudeStream(ctx, nil, &fingerprintCanceledReader{}, out, nil, true, false, map[string]string{"alias": "original"}, false, 200, func(line []byte) [][]byte { return [][]byte{line} }, nil)
	for len(out) > 0 {
		if err := (<-out).Err; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("delegated cancellation=%v", err)
		}
	}
}

type fingerprintCanceledReader struct{}

func (*fingerprintCanceledReader) Read([]byte) (int, error) { return 0, context.Canceled }

func TestClaudeFingerprintFinalCCHStableDispatchNormalization(t *testing.T) {
	body := []byte(`{"model":"claude-a","max_tokens":100,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cch=00000;"}],"messages":[{"role":"user","content":"hello"}]}`)
	first, err := signAnthropicMessagesBody(body)
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := sjson.SetBytes(body, "model", "claude-b")
	changed, _ = sjson.SetBytes(changed, "max_tokens", 200)
	second, err := signAnthropicMessagesBody(changed)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(first, "system.0.text").String() != gjson.GetBytes(second, "system.0.text").String() {
		t.Fatal("dispatch fields changed CCH")
	}
	again, err := signAnthropicMessagesBody(first)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("final-body signing not idempotent")
	}
}
