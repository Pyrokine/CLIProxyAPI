package executor

import (
	"bytes"
	"context"
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
			"account_uuid":                        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
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
