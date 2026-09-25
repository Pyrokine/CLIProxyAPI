package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const antigravityCompactionSummaryResponse = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"compact summary"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"totalTokenCount":18},"modelVersion":"gemini-3.7-flash","responseId":"summary-response"}}`

func antigravityCompactionTestOptions(opts cliproxyexecutor.Options, sessionID string) cliproxyexecutor.Options {
	opts.Metadata = map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: sessionID}
	return opts
}

func antigravityCompactionTestScope(
	t *testing.T,
	executor *AntigravityExecutor,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) helps.AntigravityCompactionScope {
	t.Helper()
	scope, errScope := executor.compactionScope(auth, req, opts, thinking.ParseSuffix(req.Model).ModelName)
	if errScope != nil {
		t.Fatalf("compactionScope: %v", errScope)
	}
	return scope
}

func TestAntigravityCompactionSessionIDPriority(t *testing.T) {
	req := cliproxyexecutor.Request{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "derived-request",
	}}
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"Session-Id": {"header-session"}},
		Metadata: map[string]any{
			cliproxyexecutor.CanonicalSessionIDMetadataKey:   "canonical-session",
			cliproxyexecutor.LCPAffinitySessionIDMetadataKey: "lcp-session",
			cliproxyexecutor.ExecutionSessionMetadataKey:     "execution-session",
			cliproxyexecutor.DerivedSessionIDMetadataKey:     "derived-option",
		},
	}
	if got := antigravityCompactionSessionID(req, opts); got != "canonical-session" {
		t.Fatalf("session ID = %q, want canonical-session", got)
	}
	delete(opts.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey)
	if got := antigravityCompactionSessionID(req, opts); got != "lcp-session" {
		t.Fatalf("session ID = %q, want lcp-session", got)
	}
	delete(opts.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
	if got := antigravityCompactionSessionID(req, opts); got != "execution:execution-session" {
		t.Fatalf("session ID = %q, want execution session", got)
	}
	delete(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey)
	if got := antigravityCompactionSessionID(req, opts); got != "derived:derived-option" {
		t.Fatalf("session ID = %q, want option-derived session", got)
	}
	delete(opts.Metadata, cliproxyexecutor.DerivedSessionIDMetadataKey)
	delete(req.Metadata, cliproxyexecutor.DerivedSessionIDMetadataKey)
	if got := antigravityCompactionSessionID(req, opts); got != "codex:header-session" {
		t.Fatalf("session ID = %q, want explicit Session-Id", got)
	}
}

func TestAntigravityCompactionScopeTracksCredentialOwnerAcrossTokenRotation(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash"}
	opts := antigravityCompactionTestOptions(cliproxyexecutor.Options{}, "session:token-rotation")
	auth := antigravityCountTokensAuth("", "stable-owner")
	auth.Metadata["refresh_token"] = "stable-refresh-token"
	firstScope := antigravityCompactionTestScope(t, executor, auth, req, opts)
	capsule, errSeal := helps.SealAntigravityCompaction("summary", firstScope)
	if errSeal != nil {
		t.Fatalf("seal capsule: %v", errSeal)
	}

	rotated := auth.Clone()
	rotated.Metadata["access_token"] = "rotated-access-token"
	rotatedScope := antigravityCompactionTestScope(t, executor, rotated, req, opts)
	if summary, errUnseal := helps.UnsealAntigravityCompaction(capsule, rotatedScope); errUnseal != nil || summary != "summary" {
		t.Fatalf("access-token rotation invalidated capsule: summary=%q err=%v", summary, errUnseal)
	}

	rebound := rotated.Clone()
	rebound.Metadata["refresh_token"] = "replacement-refresh-token"
	reboundScope := antigravityCompactionTestScope(t, executor, rebound, req, opts)
	if _, errUnseal := helps.UnsealAntigravityCompaction(capsule, reboundScope); errUnseal == nil {
		t.Fatal("credential rebind retained the old capsule")
	}
}

func TestAntigravityExecuteCompactionDispatchesOnce(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		opts    cliproxyexecutor.Options
	}{
		{
			name:    "direct endpoint",
			payload: []byte(`{"model":"gemini-3.7-flash","input":[{"type":"message","role":"user","content":"long conversation"}]}`),
			opts: cliproxyexecutor.Options{
				Alt:            "responses/compact",
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			},
		},
		{
			name:    "trigger item",
			payload: []byte(`{"model":"gemini-3.7-flash","input":[{"type":"message","role":"user","content":"long conversation"},{"type":"compaction_trigger"}]}`),
			opts: cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			var received []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != antigravityGeneratePath {
					t.Errorf("request path = %q, want %q", req.URL.Path, antigravityGeneratePath)
				}
				received, _ = io.ReadAll(req.Body)
				_ = req.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Summary-Test", "true")
				_, _ = io.WriteString(w, antigravityCompactionSummaryResponse)
			}))
			defer server.Close()

			executor := NewAntigravityExecutor(&config.Config{})
			auth := antigravityCountTokensAuth(server.URL, "compaction-"+test.name)
			req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: test.payload}
			opts := antigravityCompactionTestOptions(test.opts, "session:"+test.name)
			scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
			response, err := executor.Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("summary request count = %d, want 1", calls.Load())
			}
			if strings.Contains(string(received), "compaction_trigger") {
				t.Fatalf("summary request retained compaction trigger: %s", received)
			}
			if response.Headers.Get("X-Summary-Test") != "true" {
				t.Fatalf("summary response headers were not propagated: %v", response.Headers)
			}
			if object := gjson.GetBytes(response.Payload, "object").String(); object != "response.compaction" {
				t.Fatalf("compaction object = %q; payload=%s", object, response.Payload)
			}
			capsule := gjson.GetBytes(response.Payload, "output.0.encrypted_content").String()
			summary, errUnseal := helps.UnsealAntigravityCompaction(capsule, scope)
			if errUnseal != nil || summary != "compact summary" {
				t.Fatalf("unseal compaction = %q, %v", summary, errUnseal)
			}
			for field, want := range map[string]int64{
				"usage.input_tokens":  11,
				"usage.output_tokens": 7,
				"usage.total_tokens":  18,
			} {
				if got := gjson.GetBytes(response.Payload, field).Int(); got != want {
					t.Fatalf("compaction %s = %d, want %d; payload=%s", field, got, want, response.Payload)
				}
			}
		})
	}
}

func TestAntigravityExecuteCompactionStreamEvents(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, antigravityCompactionSummaryResponse)
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCountTokensAuth(server.URL, "compaction-stream")
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","input":[{"type":"message","role":"user","content":"long conversation"},{"type":"compaction_trigger"}]}`),
	}
	opts := antigravityCompactionTestOptions(cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}, "session:compaction-stream")
	scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", result.Headers.Get("Content-Type"))
	}
	wantEvents := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	}
	var payloads [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		payloads = append(payloads, chunk.Payload)
	}
	if len(payloads) != len(wantEvents) {
		t.Fatalf("stream chunk count = %d, want %d", len(payloads), len(wantEvents))
	}
	for index, payload := range payloads {
		if !strings.HasPrefix(string(payload), "event: "+wantEvents[index]+"\n") {
			t.Fatalf("stream chunk %d = %q", index, payload)
		}
	}
	completedLine := strings.Split(string(payloads[len(payloads)-1]), "\n")[1]
	completed := []byte(strings.TrimPrefix(completedLine, "data: "))
	for field, want := range map[string]int64{
		"response.usage.input_tokens":  11,
		"response.usage.output_tokens": 7,
		"response.usage.total_tokens":  18,
	} {
		if got := gjson.GetBytes(completed, field).Int(); got != want {
			t.Fatalf("completed %s = %d, want %d; event=%s", field, got, want, completed)
		}
	}
	capsule := gjson.GetBytes(completed, "response.output.0.encrypted_content").String()
	if summary, errUnseal := helps.UnsealAntigravityCompaction(capsule, scope); errUnseal != nil || summary != "compact summary" {
		t.Fatalf("stream capsule summary = %q, %v", summary, errUnseal)
	}
	if calls.Load() != 1 {
		t.Fatalf("summary request count = %d, want 1", calls.Load())
	}
}

func TestAntigravityExecuteStreamRejectsDirectCompaction(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	result, err := executor.ExecuteStream(
		context.Background(),
		antigravityCountTokensAuth("", "direct-compaction-stream"),
		cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"input":[]}`)},
		cliproxyexecutor.Options{Alt: "responses/compact", Stream: true},
	)
	if result != nil {
		t.Fatalf("ExecuteStream() result = %#v, want nil", result)
	}
	var status statusErr
	if !errors.As(err, &status) || status.code != http.StatusBadRequest {
		t.Fatalf("ExecuteStream() error = %#v, want status 400", err)
	}
}

func TestAntigravityExecuteRejectsCorruptedCompactionCapsules(t *testing.T) {
	var calls atomic.Int32
	roundTripper := antigravityCountTokensRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected upstream request")
	})
	executor := NewAntigravityExecutor(&config.Config{})
	bad := []byte(`{"model":"gemini-3.7-flash","input":[{"type":"compaction","encrypted_content":"cpa-ag-compact-v1:corrupted"}]}`)

	tests := []struct {
		name string
		req  cliproxyexecutor.Request
		opts cliproxyexecutor.Options
	}{
		{
			name: "request payload",
			req:  cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: bad},
			opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		},
		{
			name: "original request",
			req: cliproxyexecutor.Request{
				Model:   "gemini-3.7-flash",
				Payload: []byte(`{"model":"gemini-3.7-flash","input":[{"type":"message","role":"user","content":"hello"}]}`),
			},
			opts: cliproxyexecutor.Options{
				SourceFormat:    sdktranslator.FormatOpenAIResponse,
				OriginalRequest: bad,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := util.WithRoundTripper(context.Background(), roundTripper)
			opts := antigravityCompactionTestOptions(test.opts, "session:bad-capsule")
			_, err := executor.Execute(ctx, antigravityCountTokensAuth("", "bad-capsule"), test.req, opts)
			var status statusErr
			if !errors.As(err, &status) || status.code != http.StatusBadRequest {
				t.Fatalf("Execute() error = %#v, want status 400", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("corrupted capsules reached upstream %d time(s)", calls.Load())
	}
}

func TestAntigravityExecuteRejectsCompactionCapsuleOutsideScopeBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	roundTripper := antigravityCountTokensRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected upstream request")
	})
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{SecretKey: "test-compaction-secret"}}
	executor := NewAntigravityExecutor(cfg)
	auth := antigravityCountTokensAuth("", "capsule-owner")
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash"}
	opts := antigravityCompactionTestOptions(
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		"session:capsule-owner",
	)
	scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
	capsules := make(map[string]string)
	for _, test := range []struct {
		name  string
		scope helps.AntigravityCompactionScope
	}{
		{name: "current", scope: scope},
		{name: "expired", scope: scope},
	} {
		capsuleScope := test.scope
		if test.name == "expired" {
			capsuleScope.Now = time.Now().Add(-2 * time.Hour)
			capsuleScope.TTL = time.Hour
		}
		capsule, errSeal := helps.SealAntigravityCompaction("summary", capsuleScope)
		if errSeal != nil {
			t.Fatalf("seal %s capsule: %v", test.name, errSeal)
		}
		capsules[test.name] = capsule
	}

	reboundAuth := auth.Clone()
	reboundAuth.Metadata["access_token"] = "replacement-access-token"
	tests := []struct {
		name    string
		auth    *cliproxyauth.Auth
		model   string
		session string
		capsule string
		stream  bool
	}{
		{name: "owner", auth: antigravityCountTokensAuth("", "other-owner"), model: req.Model, session: "session:capsule-owner", capsule: capsules["current"]},
		{name: "credential rebind", auth: reboundAuth, model: req.Model, session: "session:capsule-owner", capsule: capsules["current"]},
		{name: "model", auth: auth, model: "gemini-3.7-pro", session: "session:capsule-owner", capsule: capsules["current"]},
		{name: "session", auth: auth, model: req.Model, session: "session:other", capsule: capsules["current"]},
		{name: "missing session", auth: auth, model: req.Model, capsule: capsules["current"]},
		{name: "expired", auth: auth, model: req.Model, session: "session:capsule-owner", capsule: capsules["expired"]},
		{name: "stream session", auth: auth, model: req.Model, session: "session:other", capsule: capsules["current"], stream: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := cliproxyexecutor.Request{
				Model:   test.model,
				Payload: []byte(`{"input":[{"type":"compaction","encrypted_content":"` + test.capsule + `"}]}`),
			}
			requestOpts := antigravityCompactionTestOptions(opts, test.session)
			ctx := util.WithRoundTripper(context.Background(), roundTripper)
			var err error
			if test.stream {
				_, err = executor.ExecuteStream(ctx, test.auth, request, requestOpts)
			} else {
				_, err = executor.Execute(ctx, test.auth, request, requestOpts)
			}
			var status statusErr
			if !errors.As(err, &status) || status.code != http.StatusBadRequest {
				t.Fatalf("execution error = %#v, want status 400", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("scope-invalid capsules reached upstream %d time(s)", calls.Load())
	}
}

func TestAntigravityCompactionCapsuleReplayAndSequentialCompaction(t *testing.T) {
	const oldSummary = "previous compacted context"
	tests := []struct {
		name    string
		compact bool
	}{
		{name: "next turn replay"},
		{name: "sequential compaction", compact: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var received []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				_ = req.Body.Close()
				mu.Lock()
				received = append([]byte(nil), body...)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, antigravityCompactionSummaryResponse)
			}))
			defer server.Close()

			executor := NewAntigravityExecutor(&config.Config{})
			auth := antigravityCountTokensAuth(server.URL, "capsule-"+test.name)
			opts := antigravityCompactionTestOptions(cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			}, "session:capsule-"+test.name)
			req := cliproxyexecutor.Request{Model: "gemini-3.7-flash"}
			scope := antigravityCompactionTestScope(t, executor, auth, req, opts)
			oldCapsule, errSeal := helps.SealAntigravityCompaction(oldSummary, scope)
			if errSeal != nil {
				t.Fatalf("seal old capsule: %v", errSeal)
			}
			payload := `{"model":"gemini-3.7-flash","input":[` +
				`{"type":"compaction","id":"cmp-old","encrypted_content":"` + oldCapsule + `"},` +
				`{"type":"message","role":"user","content":"continue"}`
			if test.compact {
				payload += `,{"type":"compaction_trigger"}`
			}
			req.Payload = []byte(payload + `]}`)

			response, err := executor.Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			mu.Lock()
			upstream := append([]byte(nil), received...)
			mu.Unlock()
			if !strings.Contains(string(upstream), oldSummary) {
				t.Fatalf("expanded previous summary was not sent upstream: %s", upstream)
			}
			if strings.Contains(string(upstream), oldCapsule) {
				t.Fatalf("encrypted capsule leaked upstream without expansion: %s", upstream)
			}
			if test.compact {
				capsule := gjson.GetBytes(response.Payload, "output.0.encrypted_content").String()
				summary, errUnseal := helps.UnsealAntigravityCompaction(capsule, scope)
				if errUnseal != nil || summary != "compact summary" {
					t.Fatalf("sequential compaction capsule = %q, %v", summary, errUnseal)
				}
			}
		})
	}
}
