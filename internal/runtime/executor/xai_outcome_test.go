package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type xaiOutcomeTransport func(*http.Request) (*http.Response, error)

func (f xaiOutcomeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type xaiOutcomeUsage struct{ records chan usage.Record }

func (p *xaiOutcomeUsage) HandleUsage(_ context.Context, record usage.Record) { p.records <- record }

func xaiOutcomeContext(t *testing.T) (context.Context, func() []usage.Record) {
	t.Helper()
	manager := usage.NewManager(16)
	capture := &xaiOutcomeUsage{records: make(chan usage.Record, 16)}
	manager.Register(capture)
	manager.Start(t.Context())
	t.Cleanup(manager.Stop)
	return usage.WithManager(t.Context(), manager), func() []usage.Record {
		manager.Stop()
		var records []usage.Record
		for len(capture.records) > 0 {
			records = append(records, <-capture.records)
		}
		return records
	}
}

func xaiOutcomeHTTPResponse(req *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestXAIMediaPayloadRulesReachRequest(t *testing.T) {
	for _, tc := range []struct{ format, model, path string }{
		{xaiImageHandlerType, "grok-imagine-image", xaiImagesGenerationsPath},
		{xaiImageHandlerType, "grok-imagine-image", xaiImagesEditsPath},
		{xaiVideoHandlerType, "grok-imagine-video", xaiVideosGenerationsPath},
		{xaiVideoHandlerType, "grok-imagine-video", xaiVideosEditsPath},
		{xaiVideoHandlerType, "grok-imagine-video", xaiVideosExtensionsPath},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", tc.path, enabled), func(t *testing.T) {
				match := []config.PayloadModelRule{{Name: tc.model, Protocol: "openai", FromProtocol: tc.format, Headers: map[string]string{"X-Payload-Rule": "enabled"}}}
				cfg := &config.Config{Payload: config.PayloadConfig{
					Default:     []config.PayloadRule{{Models: match, Params: map[string]any{"quality": "default quality", "count": 2}}},
					DefaultRaw:  []config.PayloadRule{{Models: match, Params: map[string]any{"metadata": `{"source":"configured"}`}}},
					Override:    []config.PayloadRule{{Models: match, Params: map[string]any{"prompt": "configured prompt", "image": map[string]any{"image_url": " https://example.invalid/configured.png "}}}},
					OverrideRaw: []config.PayloadRule{{Models: match, Params: map[string]any{"raw_marker": `{"active":true}`}}},
					Filter:      []config.PayloadFilterRule{{Models: match, Params: []string{"remove_me"}}},
				}}
				body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"original prompt","quality":"requested quality","remove_me":true,"image":{"image_url":"https://example.invalid/original.png"}}`, tc.model))
				headers := http.Header{"X-Payload-Rule": {"disabled"}, "X-Idempotency-Key": {"synthetic-idempotency"}}
				if enabled {
					headers.Set("X-Payload-Rule", "enabled")
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.format), Headers: headers, OriginalRequest: bytes.Clone(body), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1" + tc.path}}
				control := applyPayloadConfigWithRequest(cfg, tc.model, "openai", tc.format, "", body, body, tc.model, "/v1"+tc.path, headers, "xai")
				control = normalizeXAIImageRefs(control)
				var captured []byte
				calls := 0
				rt := xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != tc.path || req.Method != http.MethodPost {
						return nil, fmt.Errorf("unexpected synthetic target %s %s", req.Method, req.URL)
					}
					var err error
					captured, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					if req.Header.Get("Authorization") != "Bearer synthetic-fixture" {
						return nil, fmt.Errorf("synthetic authorization changed")
					}
					if tc.format == xaiVideoHandlerType && req.Header.Get("x-idempotency-key") != "synthetic-idempotency" {
						return nil, fmt.Errorf("video idempotency header changed")
					}
					return xaiOutcomeHTTPResponse(req, `{"data":[],"request_id":"v1"}`), nil
				})
				ctx, finish := xaiOutcomeContext(t)
				ctx = util.WithRoundTripper(ctx, rt)
				auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid", "api_key": "synthetic-fixture"}}
				if _, err := newXAIExecutor(cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: tc.model, Payload: body}, opts); err != nil {
					t.Fatal(err)
				}
				var got, want any
				if json.Unmarshal(captured, &got) != nil || json.Unmarshal(control, &want) != nil {
					t.Fatalf("invalid media JSON: got=%s control=%s", captured, control)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("payload rules did not reach request: got=%s control=%s", captured, control)
				}
				wantPrompt := "original prompt"
				if enabled {
					wantPrompt = "configured prompt"
				}
				if gjson.GetBytes(captured, "prompt").String() != wantPrompt || gjson.GetBytes(captured, "quality").String() != "requested quality" || gjson.GetBytes(captured, "image.image_url").Exists() {
					t.Fatalf("media contract changed: %s", captured)
				}
				if calls != 1 {
					t.Fatalf("requests=%d, want 1", calls)
				}
				if records := finish(); len(records) != 1 || records[0].Failed {
					t.Fatalf("media usage=%+v", records)
				}
			})
		}
	}
}

func TestXAIVideoQueryPreservesRequestIDWithoutBody(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "grok-imagine-video", Protocol: "openai"}}, Params: map[string]any{"request_id": "changed", "prompt": "configured prompt"}}}}}
	ctx, finish := xaiOutcomeContext(t)
	calls := 0
	ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/videos/original" || req.Method != http.MethodGet || req.Body != nil || req.Header.Get("x-idempotency-key") != "" {
			return nil, fmt.Errorf("query request changed: %s %s body=%v", req.Method, req.URL, req.Body)
		}
		return xaiOutcomeHTTPResponse(req, `{"status":"pending"}`), nil
	}))
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid"}}
	_, err := newXAIExecutor(cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: "grok-imagine-video", Payload: []byte(`{"request_id":"original"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiVideoHandlerType), Headers: http.Header{"X-Idempotency-Key": {"unused"}}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("requests=%d, want 1", calls)
	}
	if records := finish(); len(records) != 1 || records[0].Failed {
		t.Fatalf("query usage=%+v", records)
	}
}

func TestXAICompactUsageMatchesFinalOutcome(t *testing.T) {
	const usageJSON = `,"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	for _, mode := range []string{"ordinary", "http-trigger", "ws-sse-trigger", "ws-raw-trigger"} {
		for _, tc := range []struct {
			name, payload string
			valid         bool
		}{
			{"valid", `{"id":"resp-valid","output":[{"type":"compaction","encrypted_content":"opaque-state"}]` + usageJSON, true},
			{"empty-output", `{"id":"resp-invalid","output":[]` + usageJSON, false},
			{"missing-id", `{"output":[{"type":"compaction","encrypted_content":"opaque-state"}]` + usageJSON, false},
			{"empty-encrypted-content", `{"id":"resp-invalid","output":[{"type":"compaction","encrypted_content":""}]` + usageJSON, false},
			{"invalid-json", `{"id":`, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx, finish := xaiOutcomeContext(t)
				calls := 0
				ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses/compact" || req.Method != http.MethodPost {
						return nil, fmt.Errorf("unexpected synthetic compact target %s", req.URL)
					}
					return xaiOutcomeHTTPResponse(req, tc.payload), nil
				}))
				auth := &cliproxyauth.Auth{ID: "compact-synthetic", Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid", "api_key": "synthetic-fixture"}}
				req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var executeErr error
				var payload []byte
				if mode == "ordinary" {
					opts.Alt = "responses/compact"
					response, err := newXAIExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
					payload, executeErr = response.Payload, err
				} else {
					var result *cliproxyexecutor.StreamResult
					if strings.HasPrefix(mode, "ws-") {
						exec := newXAIWebsocketsExecutor(&config.Config{})
						exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
						exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
						if mode == "ws-raw-trigger" {
							ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
						}
						result, executeErr = exec.ExecuteStream(ctx, auth, req, opts)
					} else {
						result, executeErr = newXAIExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
					}
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							payload = append(payload, chunk.Payload...)
						}
					}
				}
				if tc.valid && executeErr != nil {
					t.Fatal(executeErr)
				}
				if !tc.valid && (executeErr == nil || len(payload) != 0) {
					t.Fatalf("invalid compact accepted: error=%v payload=%s", executeErr, payload)
				}
				if tc.valid && len(payload) == 0 {
					t.Fatal("valid compact lost response")
				}
				if calls != 1 {
					t.Fatalf("requests=%d, want 1", calls)
				}
				records := finish()
				if len(records) != 1 {
					t.Fatalf("usage records=%d, want 1", len(records))
				}
				record := records[0]
				if record.Failed == tc.valid || record.Completed != tc.valid {
					t.Fatalf("usage contradicts execution: error=%v record=%+v", executeErr, record)
				}
				if tc.name != "invalid-json" && (record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10) {
					t.Fatalf("compact usage detail changed: %+v", record.Detail)
				}
				if !tc.valid && record.Fail.Body == "" {
					t.Fatalf("failure has no reason: %+v", record)
				}
			})
		}
	}
}

func TestXAIWebsocketCompactTranscriptFailureKeepsUsageFailure(t *testing.T) {
	ctx, finish := xaiOutcomeContext(t)
	ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
	payload := fmt.Sprintf(`{"id":"resp-valid","output":[{"type":"compaction","encrypted_content":%q}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`, strings.Repeat("x", maxXAIWebsocketTranscriptBytes+1))
	ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses/compact" {
			return nil, fmt.Errorf("unexpected synthetic target %s", req.URL)
		}
		return xaiOutcomeHTTPResponse(req, payload), nil
	}))
	exec := newXAIWebsocketsExecutor(&config.Config{})
	exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid"}}
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"prompt_cache_key":"synthetic-compact","input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err == nil || result != nil || !strings.Contains(err.Error(), "transcript") {
		t.Fatalf("transcript limit not enforced: result=%v err=%v", result, err)
	}
	if records := finish(); len(records) != 1 || !records[0].Failed || records[0].Detail.TotalTokens != 10 {
		t.Fatalf("transcript failure usage=%+v", records)
	}
}

type xaiCompactTranslationFailure struct{}

func (xaiCompactTranslationFailure) ResponseTranslationError() error {
	return fmt.Errorf("synthetic compact response translation rejected")
}

type xaiOutcomeCancelBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b xaiOutcomeCancelBody) Read(data []byte) (int, error) {
	count, err := b.Reader.Read(data)
	if err == io.EOF {
		b.cancel()
	}
	return count, err
}

func (xaiOutcomeCancelBody) Close() error { return nil }

func TestXAICompactFinalizationFailurePreservesUsage(t *testing.T) {
	responseFormat := sdktranslator.Format("xai-compact-finalization-error")
	sdktranslator.Register(responseFormat, sdktranslator.FormatOpenAIResponse, nil, sdktranslator.ResponseTransform{
		NonStream: func(_ context.Context, _ string, _, _, _ []byte, param *any) []byte {
			*param = xaiCompactTranslationFailure{}
			return nil
		},
	})
	t.Cleanup(func() {
		sdktranslator.Register(responseFormat, sdktranslator.FormatOpenAIResponse, nil, sdktranslator.ResponseTransform{})
	})
	const data = `{"id":"resp-valid","output":[{"type":"compaction","encrypted_content":"opaque-state"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	var state any
	if out := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatOpenAIResponse, responseFormat, "grok-4.3", nil, nil, []byte(data), &state); len(out) != 0 || sdktranslator.ResponseTranslationError(&state) == nil {
		t.Fatal("translation rejection did not reach the SDK error channel")
	}
	for _, tc := range []struct {
		name, mode string
		cancel     bool
	}{
		{"translation", "ordinary", false},
		{"ordinary-cancellation", "ordinary", true},
		{"http-trigger-cancellation", "http-trigger", true},
		{"websocket-trigger-cancellation", "ws-raw-trigger", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, finish := xaiOutcomeContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			calls := 0
			ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses/compact" {
					return nil, fmt.Errorf("unexpected synthetic target %s", req.URL)
				}
				response := xaiOutcomeHTTPResponse(req, data)
				if tc.cancel {
					response.Body = xaiOutcomeCancelBody{Reader: strings.NewReader(data), cancel: cancel}
				}
				return response, nil
			}))
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid"}}
			req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			var executeErr error
			if tc.mode == "ordinary" {
				opts.Alt = "responses/compact"
				if !tc.cancel {
					opts.ResponseFormat = responseFormat
				}
				response, err := newXAIExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
				if len(response.Payload) != 0 {
					t.Fatalf("failed compact returned payload: %s", response.Payload)
				}
				executeErr = err
			} else {
				var result *cliproxyexecutor.StreamResult
				if tc.mode == "http-trigger" {
					result, executeErr = newXAIExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
				} else {
					exec := newXAIWebsocketsExecutor(&config.Config{})
					exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
					exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
					result, executeErr = exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(ctx), auth, req, opts)
				}
				if result != nil {
					t.Fatal("cancelled compact returned successful stream")
				}
			}
			if executeErr == nil {
				t.Fatal("compact finalization failure returned nil error")
			}
			if !tc.cancel && !strings.Contains(executeErr.Error(), "translation rejected") {
				t.Fatalf("translation failure changed: %v", executeErr)
			}
			if calls != 1 {
				t.Fatalf("requests=%d, want 1", calls)
			}
			if records := finish(); len(records) != 1 || !records[0].Failed || records[0].Completed || records[0].Detail.TotalTokens != 10 {
				t.Fatalf("finalization failure usage=%+v", records)
			}
		})
	}
}

func TestXAIDispatcherChildIdentityAcrossEvents(t *testing.T) {
	patch := `{"name":"apply_patch","arguments":{"input":"p"}}`
	ordinary := `{"name":"lookup0","arguments":{"name":"apply_patch","arguments":{"input":"ordinary"}}}`
	added := `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a","name":"n"}}`
	argsDone := fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":"a","arguments":%q}`, patch)
	done := `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"a","call_id":"c","name":"n"}}`
	terminal := `{"type":"response.completed","response":{"output":[]}}`
	terminalWithUsage := `{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`
	for _, mode := range []string{"http", "http-stream", "ws-sse", "ws-raw"} {
		for _, tc := range []struct {
			name               string
			events             []string
			fail               bool
			wantName, wantArgs string
		}{
			{"delta-child-conflict", []string{added, `{"type":"response.function_call_arguments.delta","item_id":"a","name":"lookup0","namespace":"n","delta":""}`, argsDone, done, terminal}, true, "", ""},
			{"added-child-conflict", []string{added, argsDone, done, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a","name":"lookup0","namespace":"n","arguments":""}}`, terminal}, true, "", ""},
			{"done-child-conflict", []string{added, argsDone, `{"type":"response.function_call_arguments.done","item_id":"a","name":"lookup0","namespace":"n","arguments":"{\"value\":1}"}`, done, terminal}, true, "", ""},
			{"terminal-child-conflict", []string{added, argsDone, done, `{"type":"response.completed","response":{"output":[{"type":"function_call","id":"a","call_id":"c","name":"lookup0","namespace":"n","arguments":"{}"}]}}`}, true, "", ""},
			{"sparse-consistent", []string{added, argsDone, done, terminalWithUsage}, false, "apply_patch", "p"},
			{"added-repeated", []string{added, argsDone, done, added, terminalWithUsage}, false, "apply_patch", "p"},
			{"empty-delta-repeated", []string{added, argsDone, done, `{"type":"response.function_call_arguments.delta","item_id":"a","name":"apply_patch","namespace":"n","delta":""}`, terminalWithUsage}, false, "apply_patch", "p"},
			{"ordinary-wrapper-is-literal", []string{`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a"}}`, argsDone, fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"a","call_id":"c","name":"plain","arguments":%q}}`, patch), terminalWithUsage}, false, "plain", patch},
			{"ordinary-dispatcher-child", []string{added, fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":"a","arguments":%q}`, ordinary), done, terminalWithUsage}, false, "lookup0", `{"name":"apply_patch","arguments":{"input":"ordinary"}}`},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx, finish := xaiOutcomeContext(t)
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				baseURL := "https://xai-fixture.invalid"
				if strings.HasPrefix(mode, "ws-") {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						if req.URL.Path != "/responses" {
							http.Error(w, "unexpected path", http.StatusBadRequest)
							return
						}
						conn, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
						if _, _, err = conn.ReadMessage(); err != nil {
							return
						}
						for _, event := range tc.events {
							if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
								return
							}
						}
					}))
					defer server.Close()
					baseURL = server.URL
				} else {
					ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
						if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses" {
							return nil, fmt.Errorf("unexpected synthetic target %s", req.URL)
						}
						var body strings.Builder
						for _, event := range tc.events {
							fmt.Fprintf(&body, "data: %s\n\n", event)
						}
						return xaiOutcomeHTTPResponse(req, body.String()), nil
					}))
				}
				declarations := []string{`{"type":"custom","name":"apply_patch"}`}
				for i := 0; i < 205; i++ {
					declarations = append(declarations, fmt.Sprintf(`{"type":"function","name":"lookup%d","parameters":{"type":"object"}}`, i))
				}
				request := []byte(`{"input":[],"tools":[{"type":"function","name":"plain","parameters":{"type":"object"}},{"type":"namespace","name":"n","tools":[` + strings.Join(declarations, ",") + `]}]}`)
				auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": baseURL, "api_key": "synthetic-fixture"}}
				req := cliproxyexecutor.Request{Model: "grok-4", Payload: request}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var final, output []byte
				var executeErr error
				errorCount := 0
				if mode == "http" {
					response, err := newXAIExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
					final, executeErr = response.Payload, err
				} else {
					var result *cliproxyexecutor.StreamResult
					if strings.HasPrefix(mode, "ws-") {
						exec := newXAIWebsocketsExecutor(&config.Config{})
						exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
						exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
						if mode == "ws-raw" {
							ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
						}
						result, executeErr = exec.ExecuteStream(ctx, auth, req, opts)
					} else {
						result, executeErr = newXAIExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
					}
					if executeErr != nil {
						t.Fatal(executeErr)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							executeErr = chunk.Err
							errorCount++
						}
						output = append(output, chunk.Payload...)
						for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
							payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
							if gjson.GetBytes(payload, "type").String() == "response.completed" {
								final = []byte(gjson.GetBytes(payload, "response").Raw)
							}
						}
					}
				}
				records := finish()
				if len(records) != 1 || records[0].Failed != tc.fail {
					t.Fatalf("dispatcher usage=%+v err=%v", records, executeErr)
				}
				if tc.fail {
					if executeErr == nil || !strings.Contains(executeErr.Error(), "apply_patch") || len(final) != 0 || bytes.Contains(output, []byte(`"type":"response.completed"`)) {
						t.Fatalf("child conflict accepted: final=%s output=%s err=%v", final, output, executeErr)
					}
					if mode != "http" && errorCount != 1 {
						t.Fatalf("error chunks=%d, want 1", errorCount)
					}
					return
				}
				if executeErr != nil {
					t.Fatal(executeErr)
				}
				if mode != "http" && bytes.Count(output, []byte(`"type":"response.completed"`)) != 1 {
					t.Fatalf("completion count changed: %s", output)
				}
				item := gjson.GetBytes(final, "output.0")
				if item.Get("name").String() != tc.wantName || item.Get("call_id").String() != "c" {
					t.Fatalf("child identity changed: %s", final)
				}
				if tc.wantName == "apply_patch" {
					if item.Get("type").String() != "custom_tool_call" || item.Get("namespace").String() != "n" || item.Get("input").String() != tc.wantArgs || bytes.Contains(output, []byte(`"type":"response.custom_tool_call_input.delta"`)) {
						t.Fatalf("patch lifecycle changed: final=%s output=%s", final, output)
					}
					if mode != "http" && bytes.Count(output, []byte(`"type":"response.custom_tool_call_input.done"`)) != 1 {
						t.Fatalf("patch input done count changed: %s", output)
					}
				} else if item.Get("type").String() != "function_call" || item.Get("arguments").String() != tc.wantArgs || bytes.Contains(output, []byte("custom_tool_call")) {
					t.Fatalf("ordinary wrapper changed: final=%s output=%s", final, output)
				}
			})
		}
	}
}

func TestXAIHTTPTranslationFailureRecordsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		errState bool
		output   []byte
		failed   bool
	}{
		{"empty-with-error", true, nil, true},
		{"empty-without-error", false, nil, true},
		{"output-with-error", true, []byte(`{"error":"translation rejected"}`), true},
		{"success", false, []byte(`{"content":"ok"}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			format := sdktranslator.Format("formal-xai-translation-" + tc.name)
			sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{
				NonStream: func(_ context.Context, _ string, _, _, _ []byte, param *any) []byte {
					if tc.errState {
						*param = xaiCompactTranslationFailure{}
					}
					return bytes.Clone(tc.output)
				},
			})
			t.Cleanup(func() {
				sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{})
			})
			ctx, finish := xaiOutcomeContext(t)
			calls := 0
			ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses" {
					return nil, fmt.Errorf("unexpected target %s", req.URL)
				}
				if _, err := io.ReadAll(req.Body); err != nil {
					return nil, err
				}
				return xaiOutcomeHTTPResponse(req, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"formal-xai-response\",\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"), nil
			}))
			auth := &cliproxyauth.Auth{ID: "formal-xai-translation", Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid", "api_key": "synthetic-fixture"}}
			response, err := newXAIExecutor(&config.Config{}).Execute(ctx, auth,
				cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"input":"hello"}`)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: format})
			if tc.failed {
				if statusErrCode(err) != http.StatusBadGateway || len(response.Payload) != 0 {
					t.Errorf("translation failure accepted: error=%v payload=%s", err, response.Payload)
				}
			} else if err != nil || !bytes.Equal(response.Payload, tc.output) {
				t.Errorf("successful translation changed: error=%v payload=%s", err, response.Payload)
			}
			if calls != 1 {
				t.Errorf("calls=%d, want 1", calls)
			}
			records := finish()
			if len(records) != 1 {
				t.Fatalf("usage records=%d, want 1", len(records))
			}
			record := records[0]
			if record.Failed != tc.failed || record.Completed == tc.failed || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
				t.Errorf("usage contradicts final outcome: %+v", record)
			}
			if tc.failed && record.Fail.Body == "" {
				t.Errorf("failure reason missing: %+v", record)
			}
		})
	}
}

func TestXAIHTTPStreamTranslationFailureRecordsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
		output []byte
	}{
		{"empty-with-error", true, nil},
		{"output-with-error", true, []byte(`{"error":"translation rejected"}`)},
		{"success", false, []byte(`{"content":"ok"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			format := sdktranslator.Format("formal-xai-stream-translation-" + tc.name)
			sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{
				Stream: func(_ context.Context, _ string, _, _, raw []byte, param *any) [][]byte {
					if !bytes.Contains(raw, []byte(`"type":"response.completed"`)) {
						return nil
					}
					if tc.failed {
						*param = xaiCompactTranslationFailure{}
					}
					if len(tc.output) == 0 {
						return nil
					}
					return [][]byte{bytes.Clone(tc.output)}
				},
			})
			t.Cleanup(func() {
				sdktranslator.Register(format, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{})
			})
			ctx, finish := xaiOutcomeContext(t)
			calls := 0
			ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses" {
					return nil, fmt.Errorf("unexpected target %s", req.URL)
				}
				return xaiOutcomeHTTPResponse(req, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"formal-xai-stream\",\"status\":\"completed\",\"model\":\"grok-4.3\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"), nil
			}))
			auth := &cliproxyauth.Auth{ID: "formal-xai-stream-translation", Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid", "api_key": "synthetic-fixture"}}
			result, err := newXAIExecutor(&config.Config{}).ExecuteStream(ctx, auth,
				cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"input":"hello"}`)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: format})
			if err != nil {
				t.Fatal(err)
			}
			var payload []byte
			var streamErr error
			for chunk := range result.Chunks {
				payload = append(payload, chunk.Payload...)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if tc.failed {
				if statusErrCode(streamErr) != http.StatusBadGateway || !strings.Contains(streamErr.Error(), "translation rejected") || len(payload) != 0 {
					t.Errorf("translation failure accepted: error=%v payload=%s", streamErr, payload)
				}
			} else if streamErr != nil || !bytes.Equal(payload, tc.output) {
				t.Errorf("successful translation changed: error=%v payload=%s", streamErr, payload)
			}
			if calls != 1 {
				t.Errorf("calls=%d, want 1", calls)
			}
			records := finish()
			if len(records) != 1 {
				t.Fatalf("usage records=%d, want 1", len(records))
			}
			record := records[0]
			if record.Failed != tc.failed || record.Completed == tc.failed || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 10 {
				t.Errorf("usage contradicts final outcome: %+v", record)
			}
			if tc.failed && record.Fail.Body == "" {
				t.Errorf("failure reason missing: %+v", record)
			}
		})
	}
}

func TestXAICompactPayloadOverridesFollowCleanupAcrossEntrypoints(t *testing.T) {
	for _, mode := range []string{"ordinary", "http-trigger", "ws-sse-trigger", "ws-raw-trigger"} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/override=%t", mode, override), func(t *testing.T) {
				cfg := &config.Config{}
				if override {
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "grok-4.3", NotExist: []string{"stream", "tools", "tool_choice", "top_k"}}},
						Params: map[string]any{"top_k": 10, "temperature": 0.7, "top_p": 0.6, "max_output_tokens": 48, "stop": []string{"configured"}},
					}}
				}
				ctx, finish := xaiOutcomeContext(t)
				var captured []byte
				calls := 0
				ctx = util.WithRoundTripper(ctx, xaiOutcomeTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "xai-fixture.invalid" || req.URL.Path != "/responses/compact" {
						return nil, fmt.Errorf("unexpected target %s", req.URL)
					}
					var err error
					captured, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					return xaiOutcomeHTTPResponse(req, `{"id":"resp-compact","output":[{"type":"compaction","encrypted_content":"opaque-state"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`), nil
				}))
				auth := &cliproxyauth.Auth{ID: "compact-payload-synthetic", Provider: "xai", Attributes: map[string]string{"base_url": "https://xai-fixture.invalid", "api_key": "synthetic-fixture"}}
				payload := []byte(`{"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"temperature":0.2,"top_p":0.3,"top_k":4,"max_output_tokens":12,"stop":["source"]}`)
				original := bytes.Clone(payload)
				req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: bytes.Clone(payload)}
				if mode == "ordinary" {
					opts.Alt = "responses/compact"
					if _, err := newXAIExecutor(cfg).Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				} else {
					var result *cliproxyexecutor.StreamResult
					var err error
					if strings.HasPrefix(mode, "ws-") {
						exec := newXAIWebsocketsExecutor(cfg)
						exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
						exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
						if mode == "ws-raw-trigger" {
							ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
						}
						result, err = exec.ExecuteStream(ctx, auth, req, opts)
					} else {
						result, err = newXAIExecutor(cfg).ExecuteStream(ctx, auth, req, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				}
				for _, field := range []string{"stream", "tools", "tool_choice"} {
					if gjson.GetBytes(captured, field).Exists() {
						t.Errorf("compact field %s retained: %s", field, captured)
					}
				}
				if override {
					if gjson.GetBytes(captured, "top_k").Int() != 10 || gjson.GetBytes(captured, "temperature").Float() != 0.7 || gjson.GetBytes(captured, "top_p").Float() != 0.6 || gjson.GetBytes(captured, "max_output_tokens").Int() != 48 || gjson.GetBytes(captured, "stop.0").String() != "configured" {
						t.Errorf("configured values lost: %s", captured)
					}
				} else {
					for _, field := range []string{"top_k", "temperature", "top_p", "max_output_tokens", "stop"} {
						if gjson.GetBytes(captured, field).Exists() {
							t.Errorf("source sampling %s retained: %s", field, captured)
						}
					}
				}
				if xaiInputHasItemType(captured, "compaction_trigger") || !bytes.Equal(req.Payload, original) || !bytes.Equal(opts.OriginalRequest, original) || calls != 1 {
					t.Errorf("request contract changed: calls=%d body=%s", calls, captured)
				}
				if records := finish(); len(records) != 1 || records[0].Failed || records[0].Detail.TotalTokens != 10 {
					t.Errorf("compact usage=%+v", records)
				}
			})
		}
	}
}
