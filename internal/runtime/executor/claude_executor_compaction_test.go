package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeCompactionTestModel = "claude-haiku-4-5-20251001"

func claudeCompactionTestRequest(payload []byte) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	return cliproxyexecutor.Request{Model: claudeCompactionTestModel, Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.CanonicalSessionIDMetadataKey: "conversation-a",
			cliproxyexecutor.CallerScopeMetadataKey:        "caller-a",
		},
	}
}

func claudeCompactionTestServer(t *testing.T, seen *[][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read mock upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*seen = append(*seen, body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_compact","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":11,"output_tokens":0}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
				``,
			}, "\n")))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_compact","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"Summary of the previous turns."}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":100}}`))
	}))
}

func TestClaudeCompactionTriggerReplayAndScope(t *testing.T) {
	var seen [][]byte
	server := claudeCompactionTestServer(t, &seen)
	defer server.Close()
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "credential-a", Attributes: map[string]string{
		"api_key": "test-key-a", "base_url": server.URL,
	}}
	payload := []byte(`{"model":"claude-haiku-4-5-20251001","input":[{"role":"user","content":"Summarize this conversation"},{"type":"compaction_trigger"}]}`)
	req, opts := claudeCompactionTestRequest(payload)
	result, err := executor.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute compaction: %v", err)
	}
	if got := gjson.GetBytes(result.Payload, "output.#").Int(); got != 1 {
		t.Fatalf("output count = %d, want 1: %s", got, result.Payload)
	}
	item := gjson.GetBytes(result.Payload, "output.0")
	if item.Get("type").String() != "compaction" || item.Get("encrypted_content").String() == "" {
		t.Fatalf("compaction output = %s", item.Raw)
	}
	usage := gjson.GetBytes(result.Payload, "usage")
	if usage.Get("input_tokens").Int() != 114 || usage.Get("output_tokens").Int() != 7 || usage.Get("total_tokens").Int() != 121 || usage.Get("input_tokens_details.cached_tokens").Int() != 100 {
		t.Fatalf("incorrect Responses usage: %s", usage.Raw)
	}
	if len(seen) != 1 || !bytes.Contains(seen[0], []byte("Please provide a concise and comprehensive summary")) || bytes.Contains(seen[0], []byte("compaction_trigger")) || gjson.GetBytes(seen[0], "stream").Bool() {
		t.Fatalf("invalid summary request: %q", seen)
	}

	capsule := item.Get("encrypted_content").String()
	assertReplay := func(name string, replayOpts cliproxyexecutor.Options, replayAuth *cliproxyauth.Auth, wantError bool) {
		t.Helper()
		replay := []byte(`{"input":[{"type":"compaction","encrypted_content":"` + capsule + `"},{"role":"user","content":"Continue"}]}`)
		replayReq, baseOpts := claudeCompactionTestRequest(replay)
		replayOpts.SourceFormat = baseOpts.SourceFormat
		replayOpts.ResponseFormat = baseOpts.ResponseFormat
		replayOpts.OriginalRequest = replay
		before := len(seen)
		_, replayErr := executor.Execute(context.Background(), replayAuth, replayReq, replayOpts)
		if wantError {
			if replayErr == nil || len(seen) != before {
				t.Errorf("%s: error = %v, upstream calls = %d (want %d)", name, replayErr, len(seen), before)
			}
			return
		}
		if replayErr != nil || len(seen) != before+1 || !bytes.Contains(seen[before], []byte("Summary of the previous turns.")) || bytes.Contains(seen[before], []byte(`"type":"compaction"`)) {
			t.Errorf("%s: error = %v, replay upstream request = %q", name, replayErr, seen)
		}
	}
	assertReplay("same owner", opts, auth, false)
	otherSession := opts
	otherSession.Metadata = map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "conversation-b", cliproxyexecutor.CallerScopeMetadataKey: "caller-a"}
	assertReplay("other session", otherSession, auth, true)
	otherCaller := opts
	otherCaller.Metadata = map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "conversation-a", cliproxyexecutor.CallerScopeMetadataKey: "caller-b"}
	assertReplay("other caller", otherCaller, auth, true)
	otherAuth := &cliproxyauth.Auth{ID: "credential-b", Attributes: map[string]string{"api_key": "test-key-b", "base_url": server.URL}}
	assertReplay("other credential", opts, otherAuth, true)

	foreign := []byte(`{"input":[{"type":"compaction","encrypted_content":"gAAAAA-openai-native"},{"role":"user","content":"Continue"}]}`)
	foreignReq, foreignOpts := claudeCompactionTestRequest(foreign)
	if _, errForeign := executor.Execute(context.Background(), auth, foreignReq, foreignOpts); errForeign == nil || len(seen) != 2 {
		t.Fatalf("foreign capsule: error = %v, upstream calls = %d, want 2", errForeign, len(seen))
	}
}

func TestClaudeCompactionStreamAndToolHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
	}{
		{name: "with tool declaration", tools: `,"tools":[{"type":"function","name":"check_build","parameters":{"type":"object"}}]`},
		{name: "without tool declaration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen [][]byte
			server := claudeCompactionTestServer(t, &seen)
			defer server.Close()
			executor := NewClaudeExecutor(&config.Config{
				Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: claudeCompactionTestModel}},
					Params: map[string]any{"tool_choice": map[string]any{"type": "auto"}},
				}}},
			})
			auth := &cliproxyauth.Auth{ID: "credential-a", Attributes: map[string]string{"api_key": "test-key-a", "base_url": server.URL}}
			payload := []byte(`{"model":"` + claudeCompactionTestModel + `","stream":true` + tc.tools + `,"input":[{"role":"user","content":"Check the build"},{"type":"function_call","call_id":"call_1","name":"check_build","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"build ok"},{"type":"compaction_trigger"}]}`)
			req, opts := claudeCompactionTestRequest(payload)
			opts.Stream = true
			result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("ExecuteStream compaction: %v", err)
			}
			var buf bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error: %v", chunk.Err)
				}
				buf.Write(chunk.Payload)
			}
			if !strings.Contains(buf.String(), "event: response.completed") || !strings.Contains(buf.String(), `"cached_tokens":100`) || len(seen) != 1 || gjson.GetBytes(seen[0], "stream").Bool() {
				t.Fatalf("stream frames = %q, upstream requests = %q", buf.String(), seen)
			}
			if tc.tools != "" {
				if got := gjson.GetBytes(seen[0], "tool_choice.type").String(); got != "none" {
					t.Fatalf("summary tool_choice = %q, want none", got)
				}
				if !bytes.Contains(seen[0], []byte(`"type":"tool_use"`)) || !bytes.Contains(seen[0], []byte(`"type":"tool_result"`)) {
					t.Fatalf("tool history missing: %s", seen[0])
				}
			} else if !bytes.Contains(seen[0], []byte("build ok")) || bytes.Contains(seen[0], []byte(`"type":"tool_use"`)) || bytes.Contains(seen[0], []byte(`"type":"tool_result"`)) {
				t.Fatalf("tool history not flattened: %s", seen[0])
			}
		})
	}
}

func TestClaudeCompactionRequiresSessionAndCredential(t *testing.T) {
	var seen [][]byte
	server := claudeCompactionTestServer(t, &seen)
	defer server.Close()
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{"model":"` + claudeCompactionTestModel + `","input":[{"role":"user","content":"Summarize"},{"type":"compaction_trigger"}]}`)
	req, opts := claudeCompactionTestRequest(payload)
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		opts cliproxyexecutor.Options
	}{
		{name: "no session", auth: &cliproxyauth.Auth{ID: "credential-a", Attributes: map[string]string{"api_key": "test-key-a", "base_url": server.URL}}, opts: cliproxyexecutor.Options{SourceFormat: opts.SourceFormat, OriginalRequest: payload}},
		{name: "derived session changes on replay", auth: &cliproxyauth.Auth{ID: "credential-a", Attributes: map[string]string{"api_key": "test-key-a", "base_url": server.URL}}, opts: cliproxyexecutor.Options{
			SourceFormat: opts.SourceFormat, OriginalRequest: payload,
			Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "derived-from-first-user"},
		}},
		{name: "no owner", auth: &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL}}, opts: opts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := executor.Execute(context.Background(), tc.auth, req, tc.opts); err == nil || len(seen) != 0 {
				t.Fatalf("Execute error = %v, upstream calls = %d, want preflight failure", err, len(seen))
			}
		})
	}
}

func TestClaudeCompactionCannotReplayAntigravityCapsule(t *testing.T) {
	var seen [][]byte
	server := claudeCompactionTestServer(t, &seen)
	defer server.Close()
	scope := helps.AntigravityCompactionScope{Provider: "antigravity", Model: claudeCompactionTestModel, Session: "conversation-a", Owner: "credential-a", KeyMaterial: []byte("test-key-a")}
	capsule, err := helps.SealAntigravityCompaction("Other provider summary", scope)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"input":[{"type":"compaction","encrypted_content":"` + capsule + `"},{"role":"user","content":"Continue"}]}`)
	req, opts := claudeCompactionTestRequest(payload)
	auth := &cliproxyauth.Auth{ID: "credential-a", Attributes: map[string]string{"api_key": "test-key-a", "base_url": server.URL}}
	if _, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, req, opts); err == nil || len(seen) != 0 {
		t.Fatalf("cross-provider capsule: error = %v, upstream calls = %d", err, len(seen))
	}
}
