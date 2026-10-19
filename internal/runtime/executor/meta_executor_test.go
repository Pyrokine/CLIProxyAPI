package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestMetaCredentialsRejectsDCAToken(t *testing.T) {
	cases := []struct {
		name    string
		auth    *cliproxyauth.Auth
		wantKey string
	}{
		{"DCA access token", &cliproxyauth.Auth{Provider: "meta", Metadata: map[string]any{"dca_token": "opaque-dca", "access_token": "opaque-dca"}}, ""},
		{"OAuth minted key", &cliproxyauth.Auth{Provider: "meta", Metadata: map[string]any{"dca_token": "opaque-dca", "access_token": "minted-key"}}, "minted-key"},
		{"storage DCA", &cliproxyauth.Auth{Provider: "meta", Storage: &metaauth.MetaTokenStorage{DCAToken: "opaque-dca", AccessToken: "opaque-dca"}}, ""},
		{"config API key", &cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"auth_kind": "apikey", "api_key": "minted-key"}}, "minted-key"},
		{"config DCA", &cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"auth_kind": "apikey", "api_key": "dca:key"}}, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, key := metaCredentials(test.auth)
			if key != test.wantKey {
				t.Errorf("metaCredentials() key = %q, want %q", key, test.wantKey)
			}
		})
	}
}

func TestMetaExecutorResponsesAndCredentialIsolation(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_meta","object":"response","status":"completed","model":"meta-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
	var incomingBody, authorization, clientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("request path = %s", r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		clientID = r.Header.Get("X-Client-Id")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		incomingBody = string(data)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", completed)
	}))
	defer server.Close()
	request := cliproxyexecutor.Request{Model: "meta-model", Payload: []byte(`{"model":"meta-model","input":"hello","tools":[{"type":"web_search","search_content_types":["text"]}]}`)}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
	auth := &cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	response, err := NewMetaExecutor(&config.Config{}).Execute(context.Background(), auth, request, options)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if authorization != "Bearer test-key" || clientID != "tbh:tui" {
		t.Errorf("upstream headers = Authorization %q, X-Client-Id %q", authorization, clientID)
	}
	if gjson.Get(incomingBody, "stream").Bool() != true || gjson.Get(incomingBody, "tools.0.search_content_types").Exists() {
		t.Errorf("upstream request retained unsupported fields: %s", incomingBody)
	}
	if gjson.GetBytes(response.Payload, "response.output.0.content.0.text").String() != "hello" {
		t.Errorf("translated response = %s", response.Payload)
	}
	if auth.Attributes["access_token"] != "" || auth.Metadata != nil {
		t.Fatal("executing a request changed the selected credential")
	}
}

func TestMetaExecutorPreservesForeignReasoningAtRequestPreparation(t *testing.T) {
	const museSignature = "Q-PaDg-mock-muse-spark-encrypted-reasoning-payload"
	request := cliproxyexecutor.Request{
		Model:   "meta-model",
		Payload: []byte(`{"model":"meta-model","input":[{"type":"reasoning","id":"rs_muse","summary":[],"content":[{"type":"reasoning_text","text":"thinking"}],"encrypted_content":"` + museSignature + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`),
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			prepared, err := NewMetaExecutor(&config.Config{}).prepareResponsesRequest(context.Background(), request,
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}, stream)
			if err != nil {
				t.Fatalf("prepareResponsesRequest() error: %v", err)
			}
			if got := gjson.GetBytes(prepared.body, "input.0.encrypted_content").String(); got != museSignature {
				t.Fatalf("Meta reasoning signature = %q, want %q; body=%s", got, museSignature, prepared.body)
			}
			if gjson.GetBytes(prepared.body, "input.0.summary.0.text").String() != "thinking" ||
				len(gjson.GetBytes(prepared.body, "input.0.content").Array()) != 0 ||
				gjson.GetBytes(prepared.body, "input.1.content.0.text").String() != "hello" {
				t.Fatalf("Meta reasoning replay or user content changed unexpectedly: %s", prepared.body)
			}
		})
	}
}

type metaEmptyResponseHooks struct{}

func (metaEmptyResponseHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	return body
}

func (metaEmptyResponseHooks) TranslateRequest(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, bool) ([]byte, bool) {
	return nil, false
}

func (metaEmptyResponseHooks) NormalizeResponseBefore(_ context.Context, _, _ sdktranslator.Format, _ string, _, _, body []byte, _ bool) []byte {
	return body
}

func (metaEmptyResponseHooks) TranslateResponse(_ context.Context, _, _ sdktranslator.Format, _ string, _, _, _ []byte, stream bool) ([]byte, bool) {
	return nil, !stream
}

func (metaEmptyResponseHooks) NormalizeResponseAfter(_ context.Context, _, _ sdktranslator.Format, _ string, _, _, body []byte, _ bool) []byte {
	return body
}

func TestMetaExecutorFailedTranslationIsNotRecordedAsSuccess(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_meta","object":"response","status":"completed","model":"meta-model","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
	}))
	defer server.Close()
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)
	ctx := sdktranslator.WithPluginHooks(context.Background(), metaEmptyResponseHooks{})
	_, err := NewMetaExecutor(&config.Config{}).Execute(ctx,
		&cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "meta-model", Payload: []byte(`{"model":"meta-model","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")},
	)
	if err == nil {
		t.Fatal("empty response translation reported success")
	}
	select {
	case record := <-plugin.records:
		if !record.Failed || record.Completed {
			t.Fatalf("failed translation usage = %+v, want failure", record)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestMetaExecutorStreamFramesAndTerminal(t *testing.T) {
	cases := []struct {
		name      string
		response  string
		wantError bool
	}{
		{"complete", "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"meta-model\",\"output\":[]}}\r\n\r\n", false},
		{"incomplete", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"meta-model\"}}\n\n", true},
		{"malformed", "data: {unclosed\n\n", true},
		{"provider failure", "data: {\"type\":\"response.failed\",\"response\":{\"status_code\":429,\"error\":{\"message\":\"quota exhausted\",\"resets_at\":9999999999}}}\n\n", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			auth := &cliproxyauth.Auth{Provider: "meta", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
			stream, err := NewMetaExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model: "meta-model", Payload: []byte(`{"model":"meta-model","input":"hello"}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var content strings.Builder
			var streamError error
			for chunk := range stream.Chunks {
				content.Write(chunk.Payload)
				if chunk.Err != nil {
					streamError = chunk.Err
				}
			}
			if (streamError != nil) != test.wantError {
				t.Errorf("stream error = %v, wantError = %v", streamError, test.wantError)
			}
			if !test.wantError && !strings.Contains(content.String(), "response.completed") {
				t.Errorf("missing terminal event: %s", content.String())
			}
			if test.name == "provider failure" {
				var scoped interface{ IsCredentialScoped() bool }
				if !errors.As(streamError, &scoped) || !scoped.IsCredentialScoped() {
					t.Errorf("subscription quota error should be credential-scoped: %v", streamError)
				}
			}
		})
	}
}

func TestMetaSubscriptionQuotaExecutionDisposition(t *testing.T) {
	for _, mode := range []string{"nonstream-http", "stream-http", "stream-event"} {
		for _, subscription := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/subscription=%t", mode, subscription), func(t *testing.T) {
				reset := time.Now().Add(2 * time.Hour).Unix()
				message := "request rate limited"
				if subscription {
					message = "subscription quota exhausted"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/responses" {
						t.Errorf("unexpected synthetic endpoint: %s", r.URL)
					}
					if mode == "stream-event" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"status_code\":429,\"error\":{\"message\":%q,\"resets_at\":%d}}}\n\n", message, reset)
						return
					}
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"resets_at":%d}}`, message, reset)
				}))
				t.Cleanup(server.Close)
				exec := NewMetaExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "meta", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "synthetic-meta-a", Payload: []byte(`{"input":"synthetic quota input"}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
				var executeErr error
				if mode == "nonstream-http" {
					_, executeErr = exec.Execute(t.Context(), auth, req, opts)
				} else {
					stream, err := exec.ExecuteStream(t.Context(), auth, req, opts)
					executeErr = err
					if stream != nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								if executeErr != nil {
									t.Error("duplicate upstream error")
								}
								executeErr = chunk.Err
							}
						}
					}
				}
				var status interface{ StatusCode() int }
				var outcome cliproxyexecutor.ExecutionDispositionError
				if !errors.As(executeErr, &status) || status.StatusCode() != http.StatusTooManyRequests || !errors.As(executeErr, &outcome) {
					t.Fatalf("upstream error lost status/disposition: %T %v", executeErr, executeErr)
				}
				want := cliproxyexecutor.StateEffectScopeQuotaFamily
				if subscription {
					want = cliproxyexecutor.StateEffectScopeCredential
				}
				if got := outcome.ExecutionDisposition().EffectScope; got != want {
					t.Errorf("scope=%q, want %q", got, want)
				}
				var retry interface{ RetryAfter() *time.Duration }
				if !errors.As(executeErr, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() <= 0 {
					t.Errorf("upstream retry deadline lost: %v", executeErr)
				}
			})
		}
	}
}

func TestMetaSubscriptionQuotaBlocksIndependentModels(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%t", subscription), func(t *testing.T) {
			var requests atomic.Int32
			message := "request rate limited"
			if subscription {
				message = "subscription quota exhausted"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"resets_at":%d}}`, message, time.Now().Add(2*time.Hour).Unix())
			}))
			t.Cleanup(server.Close)
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewMetaExecutor(&config.Config{}))
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "meta", Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			models := []string{"synthetic-meta-a", "synthetic-meta-b"}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "meta", []*registry.ModelInfo{
				{ID: models[0], SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}},
				{ID: models[1], SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}},
			})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			for index, model := range models {
				_, err := manager.Execute(t.Context(), []string{"meta"}, cliproxyexecutor.Request{Model: model,
					Payload: []byte(`{"input":"synthetic quota input"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
				if err == nil {
					t.Fatalf("request %d unexpectedly succeeded", index)
				}
				if index == 0 && requests.Load() != 1 {
					t.Fatalf("first request count=%d, want one; error=%v", requests.Load(), err)
				}
			}
			wantRequests := int32(2)
			if subscription {
				wantRequests = 1
			}
			if got := requests.Load(); got != wantRequests {
				t.Errorf("upstream requests=%d, want %d; credential-wide subscription quota must block the second independent model", got, wantRequests)
			}
		})
	}
}

func TestMetaExecutorFinalOutcomesPreserveUsage(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		stream     bool
	}{
		{"nonstream-success", "success", false},
		{"nonstream-source-failed", "source-failed", false},
		{"nonstream-empty-translation", "empty", false},
		{"nonstream-state-translation", "state", false},
		{"stream-success", "success", true},
		{"stream-source-failed", "source-failed", true},
		{"stream-state-translation", "state", true},
		{"stream-eof", "eof", true},
		{"stream-canceled", "canceled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const usage = `"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}`
			payload := `{"type":"response.completed","response":{"id":"synthetic-meta-outcome","status":"completed","output":[],` + usage + `}}`
			if tc.mode == "source-failed" {
				payload = `{"type":"response.failed","response":{"id":"synthetic-meta-outcome","status_code":502,"error":{"message":"synthetic source failed"},` + usage + `}}`
			} else if tc.mode == "eof" || tc.mode == "canceled" {
				payload = `{"type":"response.created","response":{"id":"synthetic-meta-outcome","status":"in_progress",` + usage + `}}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			}))
			t.Cleanup(server.Close)
			ctx, finish := xaiOutcomeContext(t)
			ctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
			if tc.mode == "empty" {
				ctx = sdktranslator.WithPluginHooks(ctx, metaEmptyResponseHooks{})
			}
			if tc.mode == "state" {
				format := sdktranslator.Format("synthetic-meta-final-outcome-" + strings.ReplaceAll(t.Name(), "/", "-"))
				failure := []byte(`{"type":"response.failed","response":{"error":{"message":"synthetic translator failed"}}}`)
				sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{
					Stream: func(_ context.Context, _ string, _, _, _ []byte, param *any) [][]byte {
						*param = xaiCompactTranslationFailure{}
						return [][]byte{append(append([]byte("data: "), failure...), []byte("\n\n")...)}
					},
					NonStream: func(_ context.Context, _ string, _, _, _ []byte, param *any) []byte {
						*param = xaiCompactTranslationFailure{}
						return bytes.Clone(failure)
					},
				})
				opts.SourceFormat = format
			}
			wantFailed := tc.mode != "success"
			exec := NewMetaExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "meta", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "synthetic-meta-model", Payload: []byte(`{"input":"synthetic final outcome"}`)}
			var resultErr error
			if !tc.stream {
				result, err := exec.Execute(ctx, auth, req, opts)
				resultErr = err
				if wantFailed && len(result.Payload) != 0 {
					t.Errorf("failed finalization returned success payload: %s", result.Payload)
				}
			} else {
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				resultErr = err
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							if resultErr != nil {
								t.Error("duplicate SDK error")
							}
							resultErr = chunk.Err
						}
						if tc.mode == "canceled" {
							cancel()
						}
						if wantFailed && bytes.Contains(chunk.Payload, []byte(`"type":"response.completed"`)) {
							t.Errorf("failed stream completed: %s", chunk.Payload)
						}
					}
				}
			}
			if (resultErr != nil) != wantFailed && !(tc.mode == "canceled" && ctx.Err() != nil) {
				t.Errorf("result error=%v, want failure=%t", resultErr, wantFailed)
			}
			records := finish()
			if len(records) != 1 {
				t.Fatalf("usage records=%d, want one", len(records))
			}
			record := records[0]
			if record.Failed != wantFailed || record.Completed == wantFailed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 {
				t.Errorf("usage failed=%t completed=%t tokens=%d/%d/%d, want failed=%t and 5/3/8", record.Failed, record.Completed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens, wantFailed)
			}
		})
	}
}

func TestMetaExecutorCompletedFormatsPreserveUsage(t *testing.T) {
	for _, wire := range []string{"sse-lf", "sse-crlf", "sse-multiline", "event-json", "bare-json"} {
		for _, outcome := range []string{"success", "empty", "state"} {
			t.Run(wire+"/"+outcome, func(t *testing.T) {
				const response = `{"id":"synthetic-meta-formats","object":"response","status":"completed","output":[],"service_tier":"priority","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
				payload := `{"type":"response.completed","response":` + response + `}`
				switch wire {
				case "sse-lf":
					payload = "data: " + payload + "\n\n"
				case "sse-crlf":
					payload = "event: response.completed\r\ndata: " + payload + "\r\n\r\n"
				case "sse-multiline":
					payload = "data: " + strings.Replace(payload, `,"response":`, ",\ndata: \"response\":", 1) + "\n\n"
				case "bare-json":
					payload = response
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprint(w, payload)
				}))
				t.Cleanup(server.Close)
				ctx, finish := xaiOutcomeContext(t)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
				if outcome == "empty" {
					ctx = sdktranslator.WithPluginHooks(ctx, metaEmptyResponseHooks{})
				}
				if outcome == "state" {
					format := sdktranslator.Format("synthetic-meta-formats-" + strings.ReplaceAll(t.Name(), "/", "-"))
					sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{
						NonStream: func(_ context.Context, _ string, _, _, _ []byte, param *any) []byte {
							*param = xaiCompactTranslationFailure{}
							return bytes.Clone([]byte(`{"type":"response.failed"}`))
						},
					})
					opts.SourceFormat = format
				}
				got, err := NewMetaExecutor(&config.Config{}).Execute(ctx,
					&cliproxyauth.Auth{ID: t.Name(), Provider: "meta", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}},
					cliproxyexecutor.Request{Model: "synthetic-meta-model", Payload: []byte(`{"input":"synthetic formats"}`)}, opts)
				wantFailed := outcome != "success"
				if (err != nil) != wantFailed || (wantFailed && len(got.Payload) != 0) {
					t.Errorf("error=%v payload=%s, want failure=%t", err, got.Payload, wantFailed)
				}
				records := finish()
				if len(records) != 1 {
					t.Fatalf("usage records=%d, want one", len(records))
				}
				record := records[0]
				if record.Failed != wantFailed || record.Completed == wantFailed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 || record.Detail.ResponseServiceTier != "priority" {
					t.Errorf("failed=%t completed=%t tokens=%d/%d/%d tier=%q, want failed=%t and 5/3/8 priority", record.Failed, record.Completed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens, record.Detail.ResponseServiceTier, wantFailed)
				}
			})
		}
	}
}

func TestMetaNotFoundCooldownReachesManager(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"model not found"}}`))
	}))
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(NewMetaExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{
		ID:       "meta-404-test",
		Provider: "meta",
		Metadata: map[string]any{
			"api_key":         "LLM|test",
			"disable_cooling": false,
			"auth_kind":       "oauth",
		},
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "meta", []*registry.ModelInfo{{ID: "muse-spark-1.3"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{
		Model:   "muse-spark-1.3",
		Payload: []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hi"}]}`),
	}
	_, _ = manager.Execute(context.Background(), []string{"meta"}, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to be registered")
	}
	state := updated.ModelStates["muse-spark-1.3"]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected model state to be unavailable, got %#v", state)
	}
	remaining := time.Until(state.NextRetryAfter)
	if remaining < 4*time.Minute || remaining > 6*time.Minute {
		t.Fatalf("expected short ~5m cooldown for Meta 404, got remaining=%v", remaining)
	}
}

func TestMetaExecutorFailedResponseObjectsPreserveOutcome(t *testing.T) {
	for _, wire := range []string{"event-json", "bare-json"} {
		t.Run(wire, func(t *testing.T) {
			const failed = `{"id":"synthetic-meta-failed-object","object":"response","status":"failed","status_code":429,"error":{"code":429,"message":"synthetic provider rejected"},"service_tier":"priority","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
			payload := failed
			if wire == "event-json" {
				payload = `{"type":"response.failed","response":` + failed + `}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, payload)
			}))
			t.Cleanup(server.Close)
			ctx, finish := xaiOutcomeContext(t)
			got, err := NewMetaExecutor(&config.Config{}).Execute(ctx,
				&cliproxyauth.Auth{ID: t.Name(), Provider: "meta", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}},
				cliproxyexecutor.Request{Model: "synthetic-meta-model", Payload: []byte(`{"input":"synthetic failed response"}`)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
			if err == nil || testExecutorStatusCode(err) != http.StatusTooManyRequests || len(got.Payload) != 0 {
				t.Errorf("error=%v status=%d payload=%s, want SDK 429 failure", err, testExecutorStatusCode(err), got.Payload)
			}
			records := finish()
			if len(records) != 1 {
				t.Fatalf("records=%d, want one", len(records))
			}
			record := records[0]
			if !record.Failed || record.Completed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 {
				t.Errorf("failed=%t completed=%t tokens=%d/%d/%d, want failure and 5/3/8", record.Failed, record.Completed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens)
			}
		})
	}
}
