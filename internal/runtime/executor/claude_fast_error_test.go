package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func claudeOAuthWireTestMetadata() map[string]any {
	return map[string]any{
		"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		claudeauth.DeviceIDsMetadataKey: []string{
			"0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

func TestClaudeExecutorFastHTTPErrorPassesThrough(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		stream     bool
		oauth      bool
		compressed bool
		betaOnly   bool
	}{
		{name: "non-stream OAuth bad request", status: http.StatusBadRequest, oauth: true},
		{name: "stream OAuth unauthorized", status: http.StatusUnauthorized, stream: true, oauth: true},
		{name: "non-stream API key forbidden", status: http.StatusForbidden},
		{
			name: "stream OAuth credits refusal", status: http.StatusTooManyRequests, stream: true, oauth: true,
			compressed: true,
		},
		{name: "non-stream OAuth server error", status: http.StatusInternalServerError, oauth: true},
		{
			name: "stream OAuth beta-only Fast refusal", status: http.StatusServiceUnavailable, stream: true,
			oauth: true, betaOnly: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var attempts atomic.Int32
				const errorJSON = `{"type":"error","error":{"type":"upstream_error","message":"Fast request rejected"}}`
				transport := protectedUtlsRoundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						attempts.Add(1)
						requestBody, errRead := io.ReadAll(req.Body)
						if errRead != nil {
							t.Fatal(errRead)
						}
						if !test.betaOnly && !bytes.Contains(requestBody, []byte(`"speed":"fast"`)) {
							t.Fatalf("upstream request does not contain speed=fast: %s", requestBody)
						}
						if test.betaOnly && bytes.Contains(requestBody, []byte(`"speed"`)) {
							t.Fatalf("beta-only Fast request unexpectedly gained speed: %s", requestBody)
						}
						var wireBetas string
						for name, values := range req.Header {
							if strings.EqualFold(name, "Anthropic-Beta") {
								wireBetas = strings.Join(values, ",")
								break
							}
						}
						if !strings.Contains(wireBetas, claudeFastModeBeta) {
							t.Fatalf("upstream request is missing %s", claudeFastModeBeta)
						}

						body := []byte(errorJSON)
						headers := http.Header{
							"Content-Type":   {"application/json"},
							"Content-Length": {"999"},
							"X-Upstream":     {"preserved"},
						}
						if test.compressed {
							var compressed bytes.Buffer
							writer := gzip.NewWriter(&compressed)
							if _, errWrite := writer.Write(body); errWrite != nil {
								t.Fatal(errWrite)
							}
							if errClose := writer.Close(); errClose != nil {
								t.Fatal(errClose)
							}
							body = compressed.Bytes()
							headers.Set("Content-Encoding", "gzip")
						}
						return &http.Response{
							StatusCode: test.status,
							Header:     headers,
							Body:       io.NopCloser(bytes.NewReader(body)),
							Request:    req,
						}, nil
					},
				)

				ctx := util.WithRoundTripper(context.Background(), transport)
				auth := &cliproxyauth.Auth{ID: "fast-error-test", Metadata: claudeOAuthWireTestMetadata()}
				if test.oauth {
					auth.Attributes = map[string]string{"api_key": "sk-ant-oat-fast-error"}
				} else {
					auth.Attributes = map[string]string{"api_key": "sk-ant-api03-fast-error"}
					auth.Metadata = nil
				}
				requestPayload := []byte(`{"model":"claude-opus-4-7","max_tokens":16,"speed":"fast","messages":[{"role":"user","content":"reply OK"}]}`)
				options := cliproxyexecutor.Options{
					Stream:         test.stream,
					SourceFormat:   sdktranslator.FormatClaude,
					ResponseFormat: sdktranslator.FormatClaude,
				}
				if test.betaOnly {
					requestPayload = []byte(`{"model":"claude-opus-4-7","max_tokens":16,"messages":[{"role":"user","content":"reply OK"}]}`)
					options.Headers = http.Header{"Anthropic-Beta": {claudeFastModeBeta}}
				}
				request := cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: requestPayload}

				executor := NewClaudeExecutor(&config.Config{})
				var errExecute error
				if test.stream {
					_, errExecute = executor.ExecuteStream(ctx, auth, request, options)
				} else {
					_, errExecute = executor.Execute(ctx, auth, request, options)
				}
				if errExecute == nil {
					t.Fatal("Fast request error = nil")
				}
				if got := attempts.Load(); got != 1 {
					t.Fatalf("upstream attempts = %d, want 1", got)
				}
				var direct *cliproxyexecutor.RequestTerminatedError
				if !errors.As(errExecute, &direct) || direct == nil {
					t.Fatalf("error = %T %v, want direct response", errExecute, errExecute)
				}
				if got := direct.StatusCode(); got != test.status {
					t.Fatalf("direct status = %d, want %d", got, test.status)
				}
				if got := string(direct.ResponseBody()); got != errorJSON {
					t.Fatalf("direct body = %q, want %q", got, errorJSON)
				}
				if got := direct.ResponseHeaders().Get("Content-Encoding"); got != "" {
					t.Fatalf("direct Content-Encoding = %q, want absent after decode", got)
				}
				if got := direct.ResponseHeaders().Get("Content-Length"); got != "" {
					t.Fatalf("direct Content-Length = %q, want absent after decode", got)
				}
				if got := direct.ResponseHeaders().Get("X-Upstream"); got != "preserved" {
					t.Fatalf("direct X-Upstream = %q, want preserved", got)
				}
				var requestScoped cliproxyexecutor.RequestScopedError
				if !errors.As(errExecute, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
					t.Fatalf("Fast direct response error = %T, want request-scoped", errExecute)
				}
			},
		)
	}
}

func TestClaudeExecutorFastSuccessfulHTTPDecodeErrorDoesNotExposeSuccessStatus(t *testing.T) {
	tests := []struct {
		name string
		run  func(
			context.Context,
			*ClaudeExecutor,
			*cliproxyauth.Auth,
			cliproxyexecutor.Request,
			cliproxyexecutor.Options,
		) error
	}{
		{
			name: "execute",
			run: func(
				ctx context.Context,
				executor *ClaudeExecutor,
				auth *cliproxyauth.Auth,
				req cliproxyexecutor.Request,
				opts cliproxyexecutor.Options,
			) error {
				_, errExecute := executor.Execute(ctx, auth, req, opts)
				return errExecute
			},
		},
		{
			name: "stream",
			run: func(
				ctx context.Context,
				executor *ClaudeExecutor,
				auth *cliproxyauth.Auth,
				req cliproxyexecutor.Request,
				opts cliproxyexecutor.Options,
			) error {
				_, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				return errStream
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var attempts atomic.Int32
				transport := protectedUtlsRoundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						attempts.Add(1)
						return &http.Response{
							StatusCode: http.StatusOK,
							Header: http.Header{
								"Content-Type":     {"application/json"},
								"Content-Encoding": {"gzip"},
							},
							Body:    io.NopCloser(strings.NewReader("not-a-gzip-stream")),
							Request: req,
						}, nil
					},
				)
				ctx := util.WithRoundTripper(context.Background(), transport)
				auth := &cliproxyauth.Auth{
					ID:         "fast-success-decode-error",
					Attributes: map[string]string{"api_key": "sk-ant-oat-fast-success-decode-error"},
					Metadata:   claudeOAuthWireTestMetadata(),
				}
				request := cliproxyexecutor.Request{
					Model:   "claude-opus-4-7",
					Payload: []byte(`{"model":"claude-opus-4-7","max_tokens":16,"speed":"fast","messages":[{"role":"user","content":"reply OK"}]}`),
				}
				errRun := test.run(
					ctx, NewClaudeExecutor(&config.Config{}), auth, request, cliproxyexecutor.Options{
						Stream:         test.name == "stream",
						SourceFormat:   sdktranslator.FormatClaude,
						ResponseFormat: sdktranslator.FormatClaude,
					},
				)
				if errRun == nil {
					t.Fatal("Fast decode error = nil")
				}
				if got := attempts.Load(); got != 1 {
					t.Fatalf("upstream attempts = %d, want 1", got)
				}
				var requestErr cliproxyexecutor.RequestScopedError
				if !errors.As(errRun, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
					t.Fatalf("Fast decode error = %T %v, want request-scoped", errRun, errRun)
				}
				var statusErr interface{ StatusCode() int }
				if !errors.As(errRun, &statusErr) || statusErr == nil {
					t.Fatalf("Fast decode error = %T %v, want status provider", errRun, errRun)
				}
				if got := statusErr.StatusCode(); got != 0 {
					t.Fatalf("Fast decode status = %d, want 0 instead of upstream success", got)
				}
			},
		)
	}
}

func TestClaudeExecutorFastTransportErrorIsRequestScoped(t *testing.T) {
	upstreamErr := errors.New("transport unavailable")
	var attempts atomic.Int32
	transport := protectedUtlsRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, upstreamErr
		},
	)
	ctx := util.WithRoundTripper(context.Background(), transport)
	auth := &cliproxyauth.Auth{
		ID:         "fast-transport-error",
		Attributes: map[string]string{"api_key": "sk-ant-oat-fast-transport"},
		Metadata:   claudeOAuthWireTestMetadata(),
	}
	request := cliproxyexecutor.Request{
		Model:   "claude-opus-4-7",
		Payload: []byte(`{"model":"claude-opus-4-7","max_tokens":16,"speed":"fast","messages":[{"role":"user","content":"reply OK"}]}`),
	}

	_, errExecute := NewClaudeExecutor(&config.Config{}).Execute(
		ctx, auth, request, cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatClaude,
			ResponseFormat: sdktranslator.FormatClaude,
		},
	)
	if !errors.Is(errExecute, upstreamErr) {
		t.Fatalf("error = %v, want wrapped transport error", errExecute)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1", got)
	}
	var requestScoped cliproxyexecutor.RequestScopedError
	if !errors.As(errExecute, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
		t.Fatalf("Fast transport error = %T, want request-scoped", errExecute)
	}
}

func TestClaudeExecutorNonFastErrorKeepsCredentialScopedBehavior(t *testing.T) {
	var attempts atomic.Int32
	transport := protectedUtlsRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			attempts.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(
					strings.NewReader(
						`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
					),
				),
				Request: req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), transport)
	auth := &cliproxyauth.Auth{
		ID:         "standard-rate-limit",
		Attributes: map[string]string{"api_key": "sk-ant-oat-standard-rate-limit"},
		Metadata:   claudeOAuthWireTestMetadata(),
	}
	request := cliproxyexecutor.Request{
		Model:   "claude-opus-4-7",
		Payload: []byte(`{"model":"claude-opus-4-7","max_tokens":16,"messages":[{"role":"user","content":"reply OK"}]}`),
	}

	_, errExecute := NewClaudeExecutor(&config.Config{}).Execute(
		ctx, auth, request, cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatClaude,
			ResponseFormat: sdktranslator.FormatClaude,
		},
	)
	var statusError interface{ StatusCode() int }
	if !errors.As(errExecute, &statusError) || statusError.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status 429", errExecute)
	}
	if _, ok := errors.AsType[*cliproxyexecutor.RequestTerminatedError](errExecute); ok {
		t.Fatal("non-Fast error unexpectedly became a direct response")
	}
	var requestScoped cliproxyexecutor.RequestScopedError
	if errors.As(errExecute, &requestScoped) && requestScoped != nil && requestScoped.IsRequestScoped() {
		t.Fatal("non-Fast rate limit unexpectedly became request-scoped")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1", got)
	}
}
