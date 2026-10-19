package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestXAIApplyPatchRejectsInvalidHistoryBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
	for _, history := range []string{
		`{"type":"custom_tool_call","name":"apply_patch","namespace":"editor","input":"patch"}`,
		`{"type":"custom_tool_call","name":"editor__apply_patch","call_id":"call-1","input":{}}`,
		`{"type":"custom_tool_call","name":"editor__apply_patch","call_id":"call-1","input":"\ud800"}`,
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", history, streaming), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				payload := `{"input":[` + history + `],"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}`
				req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(payload)}
				var err error
				if streaming {
					_, err = exec.ExecuteStream(ctx, auth, req, opts)
				} else {
					_, err = exec.Execute(ctx, auth, req, opts)
				}
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != http.StatusBadRequest {
					t.Fatalf("invalid history error=%v, want status 400", err)
				}
			})
		}
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("invalid history reached upstream %d times", calls)
	}
}
