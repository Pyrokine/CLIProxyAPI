package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type antigravityDeliveryBody struct {
	reader *strings.Reader
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *antigravityDeliveryBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.EOF
	}
}

func (b *antigravityDeliveryBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type antigravityDeliveryRoundTripper func(*http.Request) (*http.Response, error)

func (rt antigravityDeliveryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt(req)
}

type antigravityDeliveryExecutor struct {
	*runtimeexecutor.AntigravityExecutor
	body         *antigravityDeliveryBody
	requests     chan []byte
	calls        atomic.Int32
	ctx          context.Context
	sourceFormat sdktranslator.Format
}

func (e *antigravityDeliveryExecutor) ExecuteStream(
	ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	if e.sourceFormat != "" {
		translated, err := sdktranslator.TranslateRequestChecked(ctx, opts.SourceFormat, e.sourceFormat, req.Model, req.Payload, opts.Stream)
		if err != nil {
			return nil, err
		}
		req.Payload = translated
		opts.ResponseFormat = coreexecutor.ResponseFormatOrSource(opts)
		opts.SourceFormat = e.sourceFormat
	}
	ctx = util.WithRoundTripper(ctx, antigravityDeliveryRoundTripper(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		e.requests <- payload
		responseBody := io.ReadCloser(e.body)
		if e.calls.Add(1) > 1 {
			responseBody = io.NopCloser(strings.NewReader(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"next answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n\n"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: responseBody, Request: request}, nil
	}))
	e.ctx = ctx
	return e.AntigravityExecutor.ExecuteStream(ctx, auth, req, opts)
}

type antigravityDeliveryUsagePlugin struct {
	records chan coreusage.Record
}

func (p *antigravityDeliveryUsagePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	p.records <- record
}

type antigravityDeliveryWriter struct {
	*httptest.ResponseRecorder
	writeErr error
	flushErr error
	terminal bool
}

func (w *antigravityDeliveryWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("response.completed")) {
		w.terminal = true
	}
	if w.writeErr != nil && w.terminal {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(data)
}

func (w *antigravityDeliveryWriter) FlushError() error {
	if w.flushErr != nil && w.terminal {
		return w.flushErr
	}
	w.ResponseRecorder.Flush()
	return nil
}

type antigravityDeliveryLifecycleHost struct {
	completions chan pluginapi.RequestCompletion
}

func (*antigravityDeliveryLifecycleHost) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body}
}

func (*antigravityDeliveryLifecycleHost) InterceptRequestAfterAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body}
}

func (*antigravityDeliveryLifecycleHost) InterceptResponse(_ context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{Headers: req.ResponseHeaders, Body: req.Body}
}

func (*antigravityDeliveryLifecycleHost) InterceptStreamChunk(_ context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{Headers: req.ResponseHeaders, Body: req.Body}
}

func (h *antigravityDeliveryLifecycleHost) CompleteRequest(_ context.Context, completion pluginapi.RequestCompletion) {
	h.completions <- completion
}

func TestAntigravityResponsesHTTPCompletionWithoutUpstreamEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gemini-3.6-flash-high"
	const session = "offline-responses-delivery"
	const signature = "offline-terminal-thought-signature-123456789"
	failure := errors.New("synthetic downstream delivery failure")
	for _, test := range []struct {
		name         string
		writeErr     error
		flushErr     error
		sourceFormat sdktranslator.Format
	}{
		{name: "completed"},
		{name: "write failure", writeErr: failure},
		{name: "flush failure", flushErr: failure},
		{name: "chat request completed", sourceFormat: sdktranslator.FormatOpenAI},
		{name: "chat request write failure", sourceFormat: sdktranslator.FormatOpenAI, writeErr: failure},
		{name: "chat request flush failure", sourceFormat: sdktranslator.FormatOpenAI, flushErr: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalcache.ClearAntigravityReasoningReplayCache()
			t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &antigravityDeliveryBody{
				reader: strings.NewReader(
					`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"reasoning","thought":true}]}}]}}` + "\n\n" +
						`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"` + signature + `"}]}}]}}` + "\n\n" +
						`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":6}}}` + "\n\n",
				),
				ctx: ctx, closed: make(chan struct{}),
			}
			defer func() { _ = body.Close() }()
			executor := &antigravityDeliveryExecutor{
				AntigravityExecutor: runtimeexecutor.NewAntigravityExecutor(&config.Config{}),
				body:                body, requests: make(chan []byte, 4),
				sourceFormat: test.sourceFormat,
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
			manager.RegisterExecutor(executor)
			const authID = "offline-antigravity-delivery-auth"
			_, err := manager.Register(ctx, &coreauth.Auth{
				ID: authID, Provider: "antigravity", Status: coreauth.StatusActive,
				Attributes: map[string]string{"base_url": "https://antigravity.test"},
				Metadata: map[string]any{
					"access_token": "synthetic-token", "project_id": "synthetic-project",
					"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			})
			if err != nil {
				t.Fatalf("register fixture auth: %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(authID)
			usageManager := coreusage.NewManager(16)
			defer usageManager.Stop()
			plugin := &antigravityDeliveryUsagePlugin{records: make(chan coreusage.Record, 4)}
			usageManager.Register(plugin)
			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			base.UsageManager = usageManager
			host := &antigravityDeliveryLifecycleHost{completions: make(chan pluginapi.RequestCompletion, 4)}
			base.PluginHost = host
			handler := openai.NewResponsesAPIHandler(base)
			request := []byte(`{"model":"` + model + `","input":"hi","stream":true}`)
			recorder := &antigravityDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: test.writeErr, flushErr: test.flushErr}
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request)).WithContext(ctx)
			c.Request.Header.Set("Session-Id", session)
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.Responses(c)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel()
				_ = body.Close()
				<-done
				t.Fatal("Responses handler waited for upstream EOF")
			}
			wantFailed := test.writeErr != nil || test.flushErr != nil
			select {
			case record := <-plugin.records:
				if record.Failed != wantFailed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 ||
					record.Detail.ReasoningTokens != 1 || record.Detail.TotalTokens != 6 {
					t.Fatalf("usage = %+v, want failed=%t and full terminal usage", record, wantFailed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("usage did not settle after delivery")
			}
			select {
			case completion := <-host.completions:
				wantOutcome := pluginapi.RequestCompletionSucceeded
				if wantFailed {
					wantOutcome = pluginapi.RequestCompletionFailed
				}
				if completion.Outcome != wantOutcome {
					t.Fatalf("lifecycle outcome = %s, want %s", completion.Outcome, wantOutcome)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle did not finish after delivery")
			}
			if !wantFailed {
				if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "response.completed") {
					t.Fatalf("HTTP response = %d %s", recorder.Code, recorder.Body)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("upstream body remained open after transport completion")
				}
			}
			items, _, exists, errCache := internalcache.GetAntigravityReasoningReplayItemsWithSnapshotRequired(executor.ctx, model, "responses:"+session)
			if errCache != nil {
				t.Fatal(errCache)
			}
			if exists == wantFailed {
				t.Fatalf("replay exists=%t, want %t", exists, !wantFailed)
			}
			if !wantFailed {
				encoded := bytes.Join(items, []byte("\n"))
				if !bytes.Contains(encoded, []byte(signature)) {
					t.Fatalf("detached signature missing from replay: %s", encoded)
				}
			}
			select {
			case extra := <-plugin.records:
				t.Fatalf("duplicate usage record: %+v", extra)
			default:
			}
			select {
			case extra := <-host.completions:
				t.Fatalf("duplicate lifecycle completion: %+v", extra)
			default:
			}
			if !wantFailed {
				var output gjson.Result
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					if event.Get("type").String() == "response.completed" {
						output = event.Get("response.output")
					}
				}
				input := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
				for _, item := range output.Array() {
					input = append(input, json.RawMessage(item.Raw))
				}
				input = append(input, json.RawMessage(`{"role":"user","content":"next"}`))
				next, errMarshal := json.Marshal(map[string]any{"model": model, "input": input, "stream": true})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				nextRecorder := httptest.NewRecorder()
				nextContext, _ := gin.CreateTestContext(nextRecorder)
				nextContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(next)).WithContext(ctx)
				nextContext.Request.Header.Set("Session-Id", session)
				handler.Responses(nextContext)
				<-executor.requests
				prepared := <-executor.requests
				if nextRecorder.Code != http.StatusOK || !strings.Contains(nextRecorder.Body.String(), "response.completed") ||
					!bytes.Contains(prepared, []byte(signature)) || !bytes.Contains(prepared, []byte(`"text":"next"`)) {
					t.Fatalf("next request replay = %d %s; upstream=%s", nextRecorder.Code, nextRecorder.Body, prepared)
				}
			}
		})
	}
}
