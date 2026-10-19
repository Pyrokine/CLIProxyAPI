package executor

import (
	"context"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type openAICompatTTFTRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAICompatTTFTRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAICompatExecutorStreamTTFTWaitsForEffectiveToken(t *testing.T) {
	tests := []struct {
		name   string
		format sdktranslator.Format
		token  string
		finish string
	}{
		{
			name:   "text",
			format: sdktranslator.FormatOpenAI,
			token:  "data: " + `{"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n",
			finish: "stop",
		},
		{
			name:   "reasoning_content",
			format: sdktranslator.FormatOpenAI,
			token:  "data: " + `{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}` + "\n\n",
			finish: "stop",
		},
		{
			name:   "reasoning",
			format: sdktranslator.FormatOpenAI,
			token:  "data: " + `{"choices":[{"index":0,"delta":{"reasoning":"thinking"}}]}` + "\n\n",
			finish: "stop",
		},
		{
			name:   "tool_call_responses_downstream",
			format: sdktranslator.FormatOpenAIResponse,
			token:  "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"hello\"}"}}]}}]}` + "\n\n",
			finish: "tool_calls",
		},
		{
			name:   "multiline_sse_frame",
			format: sdktranslator.FormatOpenAI,
			token:  "data: {\"choices\":[\ndata: {\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n",
			finish: "stop",
		},
		{
			name:   "multiline_sse_crlf",
			format: sdktranslator.FormatOpenAI,
			token:  "event: message\r\ndata: {\"choices\":[\r\ndata: {\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\r\n\r\n",
			finish: "stop",
		},
		{
			name:   "multiline_sse_responses",
			format: sdktranslator.FormatOpenAIResponse,
			token:  "data: {\"choices\":[\ndata: {\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n",
			finish: "stop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alias := t.Name()
			capture := &captureUsagePlugin{records: make(chan coreusage.Record, 4)}
			registerTestUsagePlugin(t, capture)
			// Keep the global usage dispatcher outside the fake-clock bubble.
			coreusage.StartDefault(context.Background())

			synctest.Test(t, func(t *testing.T) {
				transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					// All sleeps use synctest's virtual clock, not wall-clock delays.
					time.Sleep(250 * time.Millisecond)
					reader, writer := io.Pipe()
					go func() {
						defer func() { _ = writer.Close() }()
						write := func(s string) bool {
							_, errWrite := io.WriteString(writer, s)
							return errWrite == nil
						}
						// Early role/empty frames and keepalives must not lock TTFT.
						if !write(": keepalive\n\ndata: {\"model\":\"glm-5.3-flash-unc\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0}}\n\n") {
							return
						}
						time.Sleep(84 * time.Second)
						if !write(tt.token) {
							return
						}
						// Later packets and the terminal event must not replace TTFT.
						time.Sleep(2 * time.Second)
						write("data: {\"model\":\"glm-5.3-flash-unc\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"" + tt.finish + "\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1,\"total_tokens\":101}}\n\ndata: [DONE]\n\n")
					}()
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"text/event-stream"}},
						Body:       reader,
						Request:    req,
					}, nil
				})
				ctx := util.WithRoundTripper(logging.WithResponseTimingHolder(context.Background()), http.RoundTripper(transport))
				ctx = coreusage.WithRequestedModelAlias(ctx, alias)
				executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}}
				payload := []byte(`{"model":"glm-5.3-flash-unc","messages":[{"role":"user","content":"hello"}]}`)
				if tt.format == sdktranslator.FormatOpenAIResponse {
					payload = []byte(`{"model":"glm-5.3-flash-unc","input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]}`)
				}
				result, errExecute := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
					Model: "glm-5.3-flash-unc", Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: tt.format, Stream: true})
				if errExecute != nil {
					t.Fatalf("ExecuteStream: %v", errExecute)
				}
				var delivered strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream: %v", chunk.Err)
					}
					delivered.Write(chunk.Payload)
				}
				if strings.HasPrefix(tt.name, "multiline_sse") && !strings.Contains(delivered.String(), "hello") {
					t.Fatalf("multiline content was not delivered: %s", delivered.String())
				}
			})
			record := awaitOpenAICompatUsage(t, capture)
			if want := 84*time.Second + 250*time.Millisecond; (time.Duration(record.TimeToFirstByteMs) * time.Millisecond) != want {
				t.Fatalf("TTFT = %v, want %v (first effective token, not role/empty/keepalive frames)", (time.Duration(record.TimeToFirstByteMs) * time.Millisecond), want)
			}
			if want := 86*time.Second + 250*time.Millisecond; record.Latency != want {
				t.Fatalf("Latency = %v, want %v", record.Latency, want)
			}
			if record.Failed {
				t.Fatal("successful stream reported as failed")
			}
			if record.ResponseModel != "glm-5.3-flash-unc" || record.Detail.OutputTokens != 1 {
				t.Fatalf("unexpected response model or usage: %+v", record)
			}
		})
	}
}

func TestOpenAICompatExecutorStreamTTFTErrorFallback(t *testing.T) {
	alias := t.Name()
	capture := &captureUsagePlugin{records: make(chan coreusage.Record, 4)}
	registerTestUsagePlugin(t, capture)
	coreusage.StartDefault(context.Background())
	synctest.Test(t, func(t *testing.T) {
		transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			time.Sleep(250 * time.Millisecond)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit"}}`)),
				Request:    req,
			}, nil
		})
		ctx := util.WithRoundTripper(logging.WithResponseTimingHolder(context.Background()), http.RoundTripper(transport))
		ctx = coreusage.WithRequestedModelAlias(ctx, alias)
		executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}}
		_, errExecute := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
			Model: "glm-5.3-flash-unc", Payload: []byte(`{"model":"glm-5.3-flash-unc","messages":[{"role":"user","content":"hello"}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
		if errExecute == nil {
			t.Fatal("expected rate limit error")
		}
	})
	record := awaitOpenAICompatUsage(t, capture)
	if !record.Failed || (time.Duration(record.TimeToFirstByteMs)*time.Millisecond) != 250*time.Millisecond {
		t.Fatalf("failed request lost first-packet fallback: failed=%v TTFT=%v", record.Failed, (time.Duration(record.TimeToFirstByteMs) * time.Millisecond))
	}
}

func awaitOpenAICompatUsage(t *testing.T, capture *captureUsagePlugin) coreusage.Record {
	t.Helper()
	select {
	case record := <-capture.records:
		return record
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
		return coreusage.Record{}
	}
}

type openAICompatObservedBody struct {
	io.ReadCloser
	closed chan struct{}
}

func (b *openAICompatObservedBody) Close() error {
	err := b.ReadCloser.Close()
	select {
	case b.closed <- struct{}{}:
	default:
	}
	return err
}

func TestOpenAICompatExecutorStreamTTFTCancellation(t *testing.T) {
	capture := &captureUsagePlugin{records: make(chan coreusage.Record, 1)}
	registerTestUsagePlugin(t, capture)
	closed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n")
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &openAICompatObservedBody{ReadCloser: reader, closed: closed}, Request: req}, nil
	})
	ctx = util.WithRoundTripper(logging.WithResponseTimingHolder(ctx), http.RoundTripper(transport))
	result, err := NewOpenAICompatExecutor("compat", &config.Config{}).ExecuteStream(ctx,
		&cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}},
		cliproxyexecutor.Request{Model: "fixture", Payload: []byte(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := <-result.Chunks
	if !ok || first.Err != nil || !strings.Contains(string(first.Payload), "first") {
		t.Fatalf("first chunk=%+v present=%t", first, ok)
	}
	cancel()
	for range result.Chunks {
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled stream retained its response body")
	}
	if calls != 1 {
		t.Fatalf("dispatches=%d, want 1", calls)
	}
	record := awaitOpenAICompatUsage(t, capture)
	if !record.Failed || record.Completed {
		t.Fatalf("cancelled stream usage=%+v", record)
	}
}

func TestOpenAICompatExecutorStreamTTFTIncompleteEventFallback(t *testing.T) {
	capture := &captureUsagePlugin{records: make(chan coreusage.Record, 1)}
	registerTestUsagePlugin(t, capture)
	coreusage.StartDefault(context.Background())
	synctest.Test(t, func(t *testing.T) {
		transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			time.Sleep(250 * time.Millisecond)
			reader, writer := io.Pipe()
			go func() {
				_, _ = io.WriteString(writer, "data: {\"choices\":[\n")
				time.Sleep(2 * time.Second)
				_ = writer.CloseWithError(io.ErrUnexpectedEOF)
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
		})
		ctx := util.WithRoundTripper(logging.WithResponseTimingHolder(context.Background()), http.RoundTripper(transport))
		result, err := NewOpenAICompatExecutor("compat", &config.Config{}).ExecuteStream(ctx,
			&cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://example.invalid/v1"}},
			cliproxyexecutor.Request{Model: "fixture", Payload: []byte(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`)},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		sawError := false
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				sawError = true
			}
		}
		if !sawError {
			t.Fatal("incomplete stream read failure was hidden")
		}
		if !logging.GetFirstResponseTokenAt(ctx).IsZero() {
			t.Fatal("incomplete JSON marked an effective token")
		}
	})
	record := awaitOpenAICompatUsage(t, capture)
	if !record.Failed || record.TimeToFirstByteMs != 250 || record.Latency != 2250*time.Millisecond {
		t.Fatalf("incomplete event lost first-byte fallback: %+v", record)
	}
}
