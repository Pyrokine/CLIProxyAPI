package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type antigravityCountTokensRoundTripFunc func(*http.Request) (*http.Response, error)

func (f antigravityCountTokensRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func antigravityCountTokensAuth(baseURL, id string) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{
		ID:       id,
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "test-token",
			"project_id":   "test-project",
			"expired":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	if baseURL != "" {
		auth.Attributes = map[string]string{"base_url": baseURL}
	}
	return auth
}

func TestAntigravityCountTokensCleansPayloadAndUsesSingleEndpoint(t *testing.T) {
	var calls atomic.Int32
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != antigravityCountTokensPath {
			t.Errorf("request path = %q, want %q", req.URL.Path, antigravityCountTokensPath)
		}
		if req.URL.RawQuery != "$alt=test-alt" {
			t.Errorf("request query = %q, want $alt=test-alt", req.URL.RawQuery)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		received, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"totalTokens":42}`)
	}))
	defer server.Close()

	payload := []byte(`{
		"project":"old-project",
		"model":"old-model",
		"request":{
			"contents":[{"role":"user","parts":[{"text":"hello"}]}],
			"safetySettings":[{"category":"x"}],
			"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},
			"labels":{"key":"value"},
			"sessionId":"session-old"
		}
	}`)
	executor := NewAntigravityExecutor(&config.Config{})
	response, err := executor.CountTokens(
		context.Background(),
		antigravityCountTokensAuth(server.URL, "count-cleanup"),
		cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, Alt: "test-alt"},
	)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("request count = %d, want 1", calls.Load())
	}
	for _, field := range []string{
		"project",
		"model",
		"request.safetySettings",
		"request.toolConfig",
		"request.labels",
		"request.sessionId",
	} {
		if gjson.GetBytes(received, field).Exists() {
			t.Fatalf("countTokens request retained %q: %s", field, received)
		}
	}
	if text := gjson.GetBytes(received, "request.contents.0.parts.0.text").String(); text != "hello" {
		t.Fatalf("request content text = %q, want hello; payload=%s", text, received)
	}
	if count := gjson.GetBytes(response.Payload, "totalTokens").Int(); count != 42 {
		t.Fatalf("raw token count = %d, want 42; payload=%s", count, response.Payload)
	}
}

func TestAntigravityCountTokensResponseFormats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"totalTokens":42}`)
	}))
	defer server.Close()

	tests := []struct {
		name           string
		responseFormat sdktranslator.Format
		assert         func(*testing.T, []byte)
	}{
		{
			name:           "Claude",
			responseFormat: sdktranslator.FormatClaude,
			assert: func(t *testing.T, payload []byte) {
				t.Helper()
				if got := gjson.GetBytes(payload, "input_tokens").Int(); got != 42 {
					t.Fatalf("Claude input_tokens = %d, want 42; payload=%s", got, payload)
				}
			},
		},
		{
			name:           "Gemini",
			responseFormat: sdktranslator.FormatGemini,
			assert: func(t *testing.T, payload []byte) {
				t.Helper()
				if got := gjson.GetBytes(payload, "totalTokens").Int(); got != 42 {
					t.Fatalf("Gemini totalTokens = %d, want 42; payload=%s", got, payload)
				}
				if got := gjson.GetBytes(payload, "promptTokensDetails.0.tokenCount").Int(); got != 42 {
					t.Fatalf("Gemini prompt token detail = %d, want 42; payload=%s", got, payload)
				}
			},
		},
		{
			name:           "OpenAI Responses raw fallback",
			responseFormat: sdktranslator.FormatOpenAIResponse,
			assert: func(t *testing.T, payload []byte) {
				t.Helper()
				if !bytes.Equal(payload, []byte(`{"totalTokens":42}`)) {
					t.Fatalf("OpenAI Responses token count payload = %s, want raw upstream payload", payload)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := NewAntigravityExecutor(&config.Config{})
			response, err := executor.CountTokens(
				context.Background(),
				antigravityCountTokensAuth(server.URL, "count-format-"+test.name),
				cliproxyexecutor.Request{
					Model:   "gemini-2.5-flash",
					Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
				},
				cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatAntigravity,
					ResponseFormat: test.responseFormat,
				},
			)
			if err != nil {
				t.Fatalf("CountTokens() error = %v", err)
			}
			test.assert(t, response.Payload)
		})
	}
}

func TestAntigravityCountTokensDoesNotFallbackAcrossEndpoints(t *testing.T) {
	wantErr := errors.New("transport failed")
	var calls atomic.Int32
	var hosts []string
	roundTripper := antigravityCountTokensRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		hosts = append(hosts, req.URL.Host)
		return nil, wantErr
	})
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	executor := NewAntigravityExecutor(&config.Config{})
	_, err := executor.CountTokens(
		ctx,
		antigravityCountTokensAuth("", "count-no-fallback"),
		cliproxyexecutor.Request{
			Model:   "gemini-2.5-flash",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("CountTokens() error = %v, want %v", err, wantErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("request count = %d, want 1", calls.Load())
	}
	if len(hosts) != 1 || hosts[0] != strings.TrimPrefix(antigravityBaseURLDaily, "https://") {
		t.Fatalf("request hosts = %v, want daily endpoint only", hosts)
	}
}

func TestAntigravityCountTokens429ClosesOnlyCredentialTransports(t *testing.T) {
	resetAntigravityTransportTestCache(t)
	firstAuth := antigravityCountTokensAuth("", "count-429-first")
	secondAuth := antigravityCountTokensAuth("", "count-429-second")
	firstBase := http.DefaultTransport.(*http.Transport).Clone()
	secondBase := http.DefaultTransport.(*http.Transport).Clone()
	firstTransport := antigravityHTTP11Transport(firstAuth, firstBase)
	_ = antigravityHTTP11Transport(firstAuth, secondBase)
	secondTransport := antigravityHTTP11Transport(secondAuth, firstBase)

	var calls atomic.Int32
	roundTripper := antigravityCountTokensRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
			Request:    req,
		}, nil
	})
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	executor := NewAntigravityExecutor(&config.Config{})
	_, err := executor.CountTokens(
		ctx,
		firstAuth,
		cliproxyexecutor.Request{
			Model:   "gemini-2.5-flash",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err == nil {
		t.Fatal("CountTokens() returned nil error for 429")
	}
	if calls.Load() != 1 {
		t.Fatalf("request count = %d, want 1", calls.Load())
	}
	if AntigravityTransportsLen() != 1 {
		t.Fatalf("transport cache length = %d, want only second credential", AntigravityTransportsLen())
	}
	if rebuilt := antigravityHTTP11Transport(firstAuth, firstBase); rebuilt == firstTransport {
		t.Fatal("429 retained the matching credential transport")
	}
	if reused := antigravityHTTP11Transport(secondAuth, firstBase); reused != secondTransport {
		t.Fatal("429 removed another credential's transport")
	}
}

func TestAntigravityCountTokensPreservesExactCancellation(t *testing.T) {
	started := make(chan struct{})
	roundTripper := antigravityCountTokensRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), roundTripper))
	result := make(chan error, 1)
	go func() {
		executor := NewAntigravityExecutor(&config.Config{})
		_, err := executor.CountTokens(
			ctx,
			antigravityCountTokensAuth("", "count-cancel"),
			cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
			},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
		)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("CountTokens request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("CountTokens() error = %#v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled CountTokens request did not return")
	}
}

func TestAntigravityCountTokensTranslatesIdenticalPairTwiceWithPluginHooks(t *testing.T) {
	hooks := &executorTranslatorHooks{}
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		received, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		_, _ = io.WriteString(w, `{"totalTokens":7}`)
	}))
	defer server.Close()

	ctx := sdktranslator.WithPluginHooks(context.Background(), hooks)
	executor := NewAntigravityExecutor(&config.Config{})
	_, err := executor.CountTokens(
		ctx,
		antigravityCountTokensAuth(server.URL, "count-plugin-hook"),
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if hooks.requestCalls.Load() != 2 {
		t.Fatalf("request hook calls = %d, want 2", hooks.requestCalls.Load())
	}
	if !gjson.GetBytes(received, "plugin_normalized").Bool() {
		t.Fatalf("request hook output was not sent upstream: %s", received)
	}
}

func TestTranslateAntigravityRequestPairTranslatesEqualDistinctInputsTwice(t *testing.T) {
	hooks := &executorTranslatorHooks{}
	ctx := sdktranslator.WithPluginHooks(context.Background(), hooks)
	originalPayload := []byte(`{"model":"gpt-5.5","input":[]}`)
	payload := bytes.Clone(originalPayload)
	executor := NewAntigravityExecutor(&config.Config{})
	original, working := executor.translateAntigravityRequestPair(
		ctx,
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload},
		nil,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatAntigravity,
		"gpt-5.5",
		originalPayload,
		payload,
		false,
	)
	if hooks.requestCalls.Load() != 2 {
		t.Fatalf("request hook calls = %d, want 2", hooks.requestCalls.Load())
	}
	if len(original) == 0 || !bytes.Equal(original, working) {
		t.Fatalf("translated pair = (%s, %s), want identical non-empty payloads", original, working)
	}
	working[0] ^= 0xff
	if bytes.Equal(original, working) {
		t.Fatal("working hook translation aliases the baseline translation")
	}
}

func TestTranslateAntigravityRequestPairClonesReusedTranslation(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	ctx := sdktranslator.WithPluginHooks(context.Background(), nil)
	executor := NewAntigravityExecutor(&config.Config{})
	original, working := executor.translateAntigravityRequestPair(
		ctx,
		cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload},
		nil,
		sdktranslator.FormatAntigravity,
		sdktranslator.FormatAntigravity,
		"gemini-2.5-flash",
		payload,
		payload,
		false,
	)
	if len(original) == 0 || !bytes.Equal(original, working) {
		t.Fatalf("translated pair = (%s, %s), want identical non-empty payloads", original, working)
	}
	working[0] ^= 0xff
	if bytes.Equal(original, working) {
		t.Fatal("working translation aliases the baseline translation")
	}
}
