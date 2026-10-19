package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator/builtin"
)

func TestReadCodexResponseBodyRequiresEOFWithinLimit(t *testing.T) {
	const limit = 64
	withinLimit := strings.Repeat("a", limit)
	data, err := readCodexResponseBody(strings.NewReader(withinLimit), limit)
	if err != nil {
		t.Fatalf("readCodexResponseBody exact limit error = %v", err)
	}
	if string(data) != withinLimit {
		t.Fatalf("readCodexResponseBody data length = %d, want %d", len(data), limit)
	}

	completedThenFailure := strings.Repeat("b", limit) + "x"
	_, err = readCodexResponseBody(strings.NewReader(completedThenFailure), limit)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("readCodexResponseBody oversized error = %v", err)
	}
}

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name   string
		status int
		quota  string
		want   time.Duration
	}{
		{name: "relative reset", status: http.StatusTooManyRequests, quota: `{"type":"usage_limit_reached","resets_in_seconds":123}`, want: 123 * time.Second},
		{name: "absolute reset wins", status: http.StatusTooManyRequests, quota: `{"type":"usage_limit_reached","resets_at":1700000300,"resets_in_seconds":1}`, want: 5 * time.Minute},
		{name: "expired absolute reset falls back", status: http.StatusTooManyRequests, quota: `{"type":"usage_limit_reached","resets_at":1699999940,"resets_in_seconds":77}`, want: 77 * time.Second},
		{name: "case and whitespace", status: http.StatusTooManyRequests, quota: `{"type":" USAGE_LIMIT_REACHED ","resets_in_seconds":30}`, want: 30 * time.Second},
		{name: "missing reset", status: http.StatusTooManyRequests, quota: `{"type":"usage_limit_reached"}`},
		{name: "nonpositive reset", status: http.StatusTooManyRequests, quota: `{"type":"usage_limit_reached","resets_at":0,"resets_in_seconds":-1}`},
		{name: "transient limit", status: http.StatusTooManyRequests, quota: `{"type":"rate_limit_error","resets_in_seconds":30}`},
		{name: "non 429", status: http.StatusBadRequest, quota: `{"type":"usage_limit_reached","resets_in_seconds":30}`},
	}
	for _, layout := range []string{"nested", "top-level"} {
		for _, test := range tests {
			t.Run(layout+"/"+test.name, func(t *testing.T) {
				body := []byte(test.quota)
				if layout == "nested" {
					body = []byte(`{"error":` + test.quota + `}`)
				}
				got := parseCodexRetryAfter(test.status, body, now)
				if test.want == 0 {
					if got != nil {
						t.Fatalf("retryAfter = %v, want nil", *got)
					}
					return
				}
				if got == nil || *got != test.want {
					t.Fatalf("retryAfter = %v, want %v", got, test.want)
				}
			})
		}
	}
}

func TestCodexUsageLimitClassificationAcrossTransports(t *testing.T) {
	quota := `{"type":" USAGE_LIMIT_REACHED ","message":"quota exhausted","resets_in_seconds":7}`
	for _, test := range []struct {
		name string
		call func() (error, bool)
	}{
		{
			name: "HTTP nested",
			call: func() (error, bool) {
				err := newCodexStatusErr(http.StatusBadRequest, []byte(`{"error":`+quota+`}`))
				return err, true
			},
		},
		{
			name: "HTTP top-level without reset",
			call: func() (error, bool) {
				err := newCodexStatusErr(http.StatusBadRequest, []byte(`{"type":"usage_limit_reached"}`))
				return err, true
			},
		},
		{
			name: "SSE response.failed",
			call: func() (error, bool) {
				return parseCodexEventError([]byte(`{"type":"response.failed","response":{"error":` + quota + `}}`))
			},
		},
		{
			name: "WebSocket error",
			call: func() (error, bool) {
				return parseCodexWebsocketError([]byte(`{"type":"error","status":429,"error":` + quota + `}`))
			},
		},
		{
			name: "WebSocket body.error",
			call: func() (error, bool) {
				return parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":` + quota + `}}`))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err, ok := test.call()
			if !ok || err == nil {
				t.Fatalf("quota error = %v, %t", err, ok)
			}
			if got := statusErrCode(err); got != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; error=%v", got, err)
			}
			classified, ok := err.(cliproxyexecutor.ExecutionDispositionError)
			if !ok {
				t.Fatal("quota error lost its execution disposition")
			}
			if scope := classified.ExecutionDisposition().EffectScope; scope != cliproxyexecutor.StateEffectScopeQuotaFamily {
				t.Fatalf("quota scope = %s, want quota family", scope)
			}
			if strings.Contains(test.name, "without reset") {
				if retry := err.(interface{ RetryAfter() *time.Duration }).RetryAfter(); retry != nil {
					t.Fatalf("retryAfter = %v, want nil", *retry)
				}
				return
			}
			retryable, supportsRetry := err.(interface{ RetryAfter() *time.Duration })
			if !supportsRetry || retryable.RetryAfter() == nil || *retryable.RetryAfter() != 7*time.Second {
				t.Fatalf("retryAfter = %#v, want 7s", err)
			}
		})
	}
}

func TestCodexRefreshUpdatesPlanTypeForRuntimeAndPersistence(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	for _, tc := range []struct {
		name      string
		oldPlan   string
		claimPlan string
		wantPlan  string
		wantFree  bool
	}{
		{name: "downgrade", oldPlan: "pro", claimPlan: "free", wantPlan: "free", wantFree: true},
		{name: "upgrade", oldPlan: "free", claimPlan: "team", wantPlan: "team"},
		{name: "missing claim", oldPlan: "pro", wantPlan: "free", wantFree: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := []byte(`{"email":"example@test.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account-refreshed","chatgpt_plan_type":"` + tc.claimPlan + `"}}`)
			idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			response := fmt.Sprintf(`{"id_token":%q,"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`, idToken)
			http.DefaultTransport = proxyTestRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.String() != "https://auth.openai.com/oauth/token" {
					return nil, fmt.Errorf("unexpected token endpoint: %s %s", req.Method, req.URL)
				}
				if err := req.ParseForm(); err != nil {
					return nil, err
				}
				if req.Form.Get("refresh_token") != "old-refresh" {
					return nil, fmt.Errorf("unexpected refresh token in mock request")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header), Request: req}, nil
			})

			storage := &codexauth.TokenStorage{PlanType: tc.oldPlan, AccessToken: "old-access", RefreshToken: "old-refresh"}
			auth := &cliproxyauth.Auth{
				Provider:   "codex",
				Metadata:   map[string]any{"refresh_token": "old-refresh", "plan_type": tc.oldPlan},
				Attributes: map[string]string{"plan_type": tc.oldPlan},
				Storage:    storage,
			}
			refreshed, err := newCodexExecutor(&config.Config{}).Refresh(context.Background(), auth)
			if err != nil {
				t.Fatalf("Refresh() error = %v", err)
			}
			if refreshed.Metadata["plan_type"] != tc.wantPlan || refreshed.Attributes["plan_type"] != tc.wantPlan {
				t.Fatalf("refreshed plan: metadata=%v attributes=%q, want %q", refreshed.Metadata["plan_type"], refreshed.Attributes["plan_type"], tc.wantPlan)
			}
			if got := isCodexFreePlanAuth(refreshed); got != tc.wantFree {
				t.Fatalf("free plan eligibility = %t, want %t", got, tc.wantFree)
			}
			if refreshed.Metadata["account_id"] != "account-refreshed" || refreshed.Metadata["access_token"] != "new-access" {
				t.Fatal("refreshed claims or access token were not applied")
			}
			if storage.PlanType != tc.oldPlan || storage.AccessToken != "old-access" {
				t.Fatal("original token storage was mutated")
			}
			updatedStorage, ok := refreshed.Storage.(*codexauth.TokenStorage)
			if !ok || updatedStorage == storage || updatedStorage.PlanType != tc.wantPlan || updatedStorage.AccessToken != "new-access" {
				t.Fatalf("refreshed token storage = %#v, want distinct storage with plan %q", refreshed.Storage, tc.wantPlan)
			}
			cliproxyauth.PrepareAuthForPersistence(refreshed)
			path := filepath.Join(t.TempDir(), "codex-test.json")
			if err := updatedStorage.SaveTokenToFile(path); err != nil {
				t.Fatal(err)
			}
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err := json.Unmarshal(file, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["plan_type"] != tc.wantPlan {
				t.Fatalf("persisted plan = %v, want %q", saved["plan_type"], tc.wantPlan)
			}
		})
	}
}

func TestCodexExecuteStreamTransformsGrokKeepaliveOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n")
		_, _ = io.WriteString(w, "event: keepalive\ndata: {\"type\":\"keepalive\",\"sequence_number\":3}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	for _, test := range []struct {
		name         string
		userAgent    string
		wantComment  bool
		wantRawEvent bool
	}{
		{name: "Grok Shell", userAgent: "grok-shell/1.0", wantComment: true},
		{name: "Grok Pager", userAgent: "grok-pager/1.0", wantComment: true},
		{name: "ordinary client", userAgent: "curl/8.7.1", wantRawEvent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exec := newCodexExecutor(&config.Config{})
			result, errExecute := exec.ExecuteStream(
				context.Background(),
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
				cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)},
				cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAIResponse,
					Headers:      http.Header{"User-Agent": {test.userAgent}},
				},
			)
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}
			var output bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error = %v", chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			got := output.String()
			if strings.Contains("\n"+got, "\n: keepalive\n") != test.wantComment {
				t.Fatalf("keepalive comment mismatch: %s", got)
			}
			hasRawEvent := strings.Contains(got, `"type":"keepalive"`) || strings.Contains(got, "event: keepalive")
			if hasRawEvent != test.wantRawEvent {
				t.Fatalf("raw keepalive mismatch: %s", got)
			}
		})
	}
}

func TestCodexExecuteRetriesHTTP408Once(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts == 1 {
					w.WriteHeader(http.StatusRequestTimeout)
					_, _ = w.Write([]byte(`{"error":{"message":"stream error: stream disconnected before completion: stream closed before response.completed"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	resp, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !bytes.Contains(resp.Payload, []byte(`"gpt-5.5"`)) {
		t.Fatalf("response payload missing model: %s", string(resp.Payload))
	}
}

func TestCodexExecuteRequiresCompleteSSEFrames(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "multiline data",
			body: "data: {\"type\":\ndata: \"response.completed\",\"response\":{\"id\":\"resp_multiline\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[]}}\n\n",
		},
		{
			name:    "delimiterless completion",
			body:    "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_truncated\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[]}}",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, tt.body)
						},
					),
				)
				defer server.Close()

				exec := newCodexExecutor(&config.Config{})
				resp, err := exec.Execute(
					context.Background(),
					&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
					cliproxyexecutor.Request{
						Model:   "gpt-5.5",
						Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`),
					},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
				)
				if tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), "incomplete SSE frame") {
						t.Fatalf("Execute error = %v, want incomplete SSE frame", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Execute error: %v", err)
				}
				if !bytes.Contains(resp.Payload, []byte(`"resp_multiline"`)) {
					t.Fatalf("response payload = %s, want multiline completion", resp.Payload)
				}
			},
		)
	}
}

func TestCodexExecuteRecoversWhenCompletedEventIsMissing(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.added\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.done\"}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	resp, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !bytes.Contains(resp.Payload, []byte(`"gpt-5.5"`)) {
		t.Fatalf("response payload missing model: %s", string(resp.Payload))
	}
	if !bytes.Contains(resp.Payload, []byte(`"ok"`)) {
		t.Fatalf("response payload missing recovered text: %s", string(resp.Payload))
	}
}

func TestCodexExecuteStreamPropagatesUpstreamEventError(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_err\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Your input exceeds the context window of this model. Please adjust your input and try again.\",\"code\":\"context_length_exceeded\"}}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var chunks [][]byte
	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
		if len(chunk.Payload) > 0 {
			chunks = append(chunks, append([]byte(nil), chunk.Payload...))
		}
	}
	if streamErr == nil {
		t.Fatal("expected stream error, got nil")
	}
	if statusErrCode(streamErr) != http.StatusBadRequest {
		t.Fatalf("statusErrCode = %d, want %d", statusErrCode(streamErr), http.StatusBadRequest)
	}
	if !bytes.Contains([]byte(streamErr.Error()), []byte("context_length_exceeded")) {
		t.Fatalf("stream error = %v, want context_length_exceeded", streamErr)
	}
	joined := bytes.Join(chunks, nil)
	if !bytes.Contains(joined, []byte("event: message_start")) {
		t.Fatalf("stream payload missing message_start: %s", string(joined))
	}
	if bytes.Contains(joined, []byte("event: message_stop")) {
		t.Fatalf("stream payload should not include message_stop on upstream error: %s", string(joined))
	}
}

func TestCodexExecuteStreamAcceptsMultilineSSEData(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(
					w,
					"data: {\"type\":\ndata: \"response.completed\",\"response\":{\"id\":\"resp_multiline\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n",
				)
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if !bytes.Contains(payload, []byte(`"response.completed"`)) {
		t.Fatalf("stream payload missing completed event: %q", payload)
	}
}

func TestCodexExecuteStreamRejectsDelimiterlessCompletedEvent(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(
					w,
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_truncated\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}",
				)
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payload = append(payload, chunk.Payload...)
	}
	if streamErr == nil {
		t.Fatal("stream error = nil, want delimiterless SSE error")
	}
	if bytes.Contains(payload, []byte(`"response.completed"`)) {
		t.Fatalf("delimiterless completion was forwarded: %q", payload)
	}
}

func TestCodexExecuteStreamRejectsMalformedDataFrame(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(
					w,
					"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bad\",\"model\":\"gpt-5.5\"}}\n\n",
				)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":}\n\n")
				_, _ = io.WriteString(
					w,
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bad\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n",
				)
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || statusErrCode(streamErr) != http.StatusBadGateway {
		t.Fatalf("stream error = %v, want 502", streamErr)
	}
	if !strings.Contains(streamErr.Error(), "malformed stream data") {
		t.Fatalf("stream error = %v, want malformed stream data", streamErr)
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestCodexExecuteStreamIgnoresTransportErrorAfterCompleted(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("response writer does not support hijacking")
				}
				conn, rw, errHijack := hijacker.Hijack()
				if errHijack != nil {
					t.Fatalf("Hijack error: %v", errHijack)
				}
				defer func() { _ = conn.Close() }()

				payload := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_partial\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"
				_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
				_, _ = rw.WriteString(strconv.FormatInt(int64(len(payload)), 16) + "\r\n" + payload + "\r\n")
				_ = rw.Flush()
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payload = append(payload, chunk.Payload...)
	}
	if streamErr != nil {
		t.Fatalf("stream error = %v, want terminal completion to win", streamErr)
	}
	if !bytes.Contains(payload, []byte(`"response.completed"`)) {
		t.Fatalf("stream payload missing completed event: %q", payload)
	}

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("usage record Failed = true, want successful terminal event: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestCodexExecuteIncompleteResponseIsSuccessful(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(
					w,
					"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete\",\"model\":\"gpt-5.5\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n",
				)
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	resp, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !bytes.Contains(resp.Payload, []byte(`"status":"incomplete"`)) {
		t.Fatalf("response payload missing incomplete status: %s", resp.Payload)
	}
}

func TestCodexExecuteMissingCompletionIsRequestScoped(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	_, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(err, &requestErr) || !requestErr.IsRequestScoped() {
		t.Fatalf("Execute error = %T %v, want request-scoped error", err, err)
	}
}

func TestCodexExecuteStreamMissingCompletionIsRequestScoped(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("stream error = nil, want incomplete stream error")
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(streamErr, &requestErr) || !requestErr.IsRequestScoped() {
		t.Fatalf("stream error = %T %v, want request-scoped error", streamErr, streamErr)
	}
}

func TestCodexExecuteStreamRecoversWhenCompletedEventIsMissing(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.added\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.done\"}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		joined = append(joined, chunk.Payload...)
	}
	if !bytes.Contains(joined, []byte("event: message_start")) {
		t.Fatalf("stream payload missing message_start: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("event: content_block_delta")) {
		t.Fatalf("stream payload missing content_block_delta: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("\"text\":\"ok\"")) {
		t.Fatalf("stream payload missing recovered text: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("event: message_stop")) {
		t.Fatalf("stream payload missing message_stop: %s", string(joined))
	}
}
