package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestInjectClaudeDiagnosticsMatchesNativeFieldOrderAndContinuity(t *testing.T) {
	body := []byte(`{"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"max_tokens":1,"messages":[]}`)
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{ID: "credential-diagnostics-order-" + testID}
	first, state := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	wantOrder := `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"diagnostics":{"previous_message_id":null},"max_tokens"`
	if !bytes.Contains(first, []byte(wantOrder)) {
		t.Fatalf("diagnostics field order differs from native: %s", first)
	}
	if got := gjson.GetBytes(first, "diagnostics.previous_message_id"); got.Type != gjson.Null {
		t.Fatalf("first previous_message_id = %s, want null", got.Raw)
	}

	commitClaudeDiagnostics(state, "msg_01ABCDEF0123456789ABCDEFG")
	second, _ := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	if got := gjson.GetBytes(
		second, "diagnostics.previous_message_id",
	).String(); got != "msg_01ABCDEF0123456789ABCDEFG" {
		t.Fatalf("second previous_message_id = %q, want committed upstream ID", got)
	}
}

func TestClaudeExecutorDiagnosticsAdvancesAfterSuccessfulResponse(t *testing.T) {
	var previousValues []gjson.Result
	var betaValues []string
	call := 0
	ctx := util.WithRoundTripper(
		context.Background(),
		protectedUtlsRoundTripFunc(
			func(req *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatal(errRead)
				}
				previousValues = append(previousValues, gjson.GetBytes(body, "diagnostics.previous_message_id"))
				betaValues = append(betaValues, headerValueCaseInsensitive(req.Header, "Anthropic-Beta"))
				call++
				response := `{"id":"msg_diagnostics_` + string(rune('0'+call)) + `","type":"message","model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(response)),
					Request:    req,
				}, nil
			},
		),
	)
	deviceIDs := []string{"0000000000000000000000000000000000000000000000000000000000000000"}
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:         "diagnostics-live-path-" + testID,
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "sk-ant-oat-diagnostics-live-path", "auth_kind": "oauth"},
		Metadata: map[string]any{
			"account_uuid":                  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			claudeauth.DeviceIDsMetadataKey: deviceIDs,
		},
	}
	executor := NewClaudeExecutor(&config.Config{})
	request := cliproxyexecutor.Request{
		Model:   "claude-opus-4-7",
		Payload: []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"x"}],"max_tokens":16}`),
	}
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "diagnostics-conversation-" + testID,
		},
	}
	for turn := range 2 {
		if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		if turn == 0 {
			auth.Attributes["api_key"] = "sk-ant-oat-diagnostics-live-path-rotated"
		}
	}
	if len(previousValues) != 2 || previousValues[0].Type != gjson.Null || previousValues[0].Raw != "null" {
		t.Fatalf("first diagnostics value = %#v, want explicit null", previousValues)
	}
	if got := previousValues[1].String(); got != "msg_diagnostics_1" {
		t.Fatalf("second diagnostics previous_message_id = %q, want first upstream response ID", got)
	}
	wantTrailer := claudeExtendedCacheTTLBeta + "," + claudeCacheDiagnosisBeta
	for turn, betas := range betaValues {
		if !strings.HasSuffix(betas, wantTrailer) {
			t.Fatalf("turn %d Anthropic-Beta = %q, want diagnostics trailer %q", turn+1, betas, wantTrailer)
		}
	}
}

func TestClaudeMessageIDFromSSECommitsOnlyCompletedMessage(t *testing.T) {
	complete := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_complete\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if got := claudeMessageIDFromSSE(complete); got != "msg_complete" {
		t.Fatalf("completed SSE message ID = %q, want msg_complete", got)
	}
	incomplete := []byte(strings.Replace(
		string(complete), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1,
	))
	if got := claudeMessageIDFromSSE(incomplete); got != "" {
		t.Fatalf("incomplete SSE message ID = %q, want empty", got)
	}
}

func TestClaudeExecutorContinuityAdvancesRequestIDAndPromptIDInBillingHeader(t *testing.T) {
	var capturedBillingHeaders []string
	call := 0
	transport := protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		capturedBillingHeaders = append(capturedBillingHeaders, gjson.GetBytes(body, "system.0.text").String())
		call++
		response := fmt.Sprintf(`{"id":"msg_turn_%d","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`, call)
		header := http.Header{
			"Content-Type": {"application/json"},
			"request-id":   {fmt.Sprintf("req_upstream_turn_%d", call)},
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    req,
		}, nil
	})
	ctx := util.WithRoundTripper(context.Background(), transport)
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:         "continuity-test-" + testID,
		Attributes: map[string]string{"api_key": "sk-ant-oat-continuity-test"},
		Metadata: map[string]any{
			"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			claudeauth.DeviceIDsMetadataKey: []string{
				"0000000000000000000000000000000000000000000000000000000000000000",
			},
		},
	}
	executor := NewClaudeExecutor(&config.Config{})
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "continuity-conv-" + testID,
		},
	}

	requests := []struct {
		name    string
		request cliproxyexecutor.Request
		options cliproxyexecutor.Options
	}{
		{
			name: "turn 1",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"}],"max_tokens":100}`,
			)},
			options: options,
		},
		{
			name: "turn 2",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn 2 prompt"}],"max_tokens":100}`,
			)},
			options: options,
		},
		{
			name: "turn 2 tool continuation",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn 2 prompt"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"tool output"}]}],"max_tokens":100}`,
			)},
			options: options,
		},
		{
			name: "probe",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"probe"}],"max_tokens":1}`,
			)},
			options: options,
		},
		{
			name: "subagent",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"subagent prompt"}],"max_tokens":100}`,
			)},
			options: cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatClaude,
				Headers:      http.Header{"X-Claude-Code-Agent-Id": {"subagent-worker-1"}},
				Metadata: map[string]any{
					cliproxyexecutor.ExecutionSessionMetadataKey: "continuity-subagent-" + testID,
				},
			},
		},
		{
			name: "turn 5",
			request: cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(
				`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 5 prompt"}],"max_tokens":100}`,
			)},
			options: options,
		},
	}
	for _, request := range requests {
		if _, err := executor.Execute(ctx, auth, request.request, request.options); err != nil {
			t.Fatalf("%s failed: %v", request.name, err)
		}
	}

	if len(capturedBillingHeaders) != 6 {
		t.Fatalf("captured %d billing headers, want 6", len(capturedBillingHeaders))
	}
	h1 := capturedBillingHeaders[0]
	if !strings.Contains(h1, "cc_version=2.1.258.") || !strings.Contains(h1, "cc_entrypoint=cli;") || !strings.Contains(h1, "cch=") {
		t.Fatalf("h1 invalid: %s", h1)
	}
	if strings.Contains(h1, "cc_prev_req=") {
		t.Fatalf("h1 must not contain cc_prev_req: %s", h1)
	}
	prompt1 := extractTag(h1, "cc_prompt_id=")
	if prompt1 == "" {
		t.Fatalf("h1 must contain cc_prompt_id: %s", h1)
	}

	h2 := capturedBillingHeaders[1]
	if !strings.Contains(h2, "cc_prev_req=req_upstream_turn_1;") {
		t.Fatalf("h2 must contain cc_prev_req=req_upstream_turn_1;, got: %s", h2)
	}
	prompt2 := extractTag(h2, "cc_prompt_id=")
	if prompt2 == "" || prompt2 == prompt1 {
		t.Fatalf("h2 promptID = %q, want non-empty and different from h1 %q", prompt2, prompt1)
	}

	h2Tool := capturedBillingHeaders[2]
	if !strings.Contains(h2Tool, "cc_prev_req=req_upstream_turn_2;") {
		t.Fatalf("h2Tool must contain cc_prev_req=req_upstream_turn_2;, got: %s", h2Tool)
	}
	if prompt2Tool := extractTag(h2Tool, "cc_prompt_id="); prompt2Tool != prompt2 {
		t.Fatalf("h2Tool promptID = %q, want turn 2 promptID %q", prompt2Tool, prompt2)
	}

	hProbe := capturedBillingHeaders[3]
	if strings.Contains(hProbe, "cc_prev_req=") || strings.Contains(hProbe, "cc_prompt_id=") {
		t.Fatalf("probe must not contain continuity tags: %s", hProbe)
	}

	hSubagent := capturedBillingHeaders[4]
	if !strings.Contains(hSubagent, "cc_is_subagent=true;") || extractTag(hSubagent, "cc_prompt_id=") == "" {
		t.Fatalf("subagent billing header missing identity or prompt ID: %s", hSubagent)
	}

	h5 := capturedBillingHeaders[5]
	if !strings.Contains(h5, "cc_prev_req=req_upstream_turn_3;") {
		t.Fatalf("h5 must bypass probe request and contain cc_prev_req=req_upstream_turn_3;, got: %s", h5)
	}
	if extractTag(h5, "cc_prompt_id=") == "" {
		t.Fatalf("h5 must contain cc_prompt_id: %s", h5)
	}
}
