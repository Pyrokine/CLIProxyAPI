package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestOpenAICompatEffectiveTokenFrames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    bool
	}{
		{"role announcement", `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"empty delta", `data: {"choices":[{"delta":{}}]}`, false},
		{"usage only", `data: {"choices":[],"usage":{"completion_tokens":1}}`, false},
		{"text", `data: {"choices":[{"delta":{"content":"hello"}}]}`, true},
		{"reasoning", `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, true},
		{"tool arguments", `data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, true},
		{"finish reason", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, true},
		{"completed message", `{"choices":[{"message":{"content":"hello"}}]}`, true},
		{"error", `data: {"error":{"message":"denied"}}`, true},
		{"done", "data: [DONE]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOpenAICompatTokenEvent([]byte(tc.payload)); got != tc.want {
				t.Fatalf("isOpenAICompatTokenEvent(%s) = %t, want %t", tc.payload, got, tc.want)
			}
		})
	}
}

func TestOpenAICompatStreamRecordsEffectiveTokenAfterRole(t *testing.T) {
	roleSent := make(chan struct{})
	releaseToken := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseToken) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		close(roleSent)
		select {
		case <-releaseToken:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer func() { release(); upstream.Close() }()

	ctx := logging.WithResponseTimingHolder(context.Background())
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": "synthetic"}}
	req := cliproxyexecutor.Request{Model: "upstream-model", Payload: []byte(`{"model":"upstream-model","messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true}
	result, err := NewOpenAICompatExecutor("compat", &config.Config{}).ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	<-roleSent
	deadline := time.After(time.Second)
	for logging.GetFirstResponseByteAt(ctx).IsZero() {
		select {
		case <-deadline:
			t.Fatal("role frame was not observed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	firstByte := logging.GetFirstResponseByteAt(ctx)
	if got := logging.GetFirstResponseTokenAt(ctx); !got.IsZero() {
		t.Fatalf("role frame marked a token at %s", got)
	}
	time.Sleep(15 * time.Millisecond)
	release()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk: %v", chunk.Err)
		}
	}
	firstToken := logging.GetFirstResponseTokenAt(ctx)
	if !firstToken.After(firstByte) {
		t.Fatalf("first token = %s, first packet = %s", firstToken, firstByte)
	}
	reporter := &usageReporter{requestedAt: firstByte.Add(-time.Second)}
	record := reporter.finalizeRecord(ctx, usage.Record{RequestedAt: reporter.requestedAt})
	if want := firstToken.Sub(reporter.requestedAt).Milliseconds(); record.TimeToFirstByteMs != want {
		t.Fatalf("stream TTFT = %d ms, want %d ms", record.TimeToFirstByteMs, want)
	}
}
