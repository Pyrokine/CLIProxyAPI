package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

func TestInternalModelExecutionPathAndImageProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		entry  string
		exit   string
		stream bool
	}{
		{name: "image entry non-stream", entry: "openai-image"},
		{name: "image exit non-stream", entry: "openai", exit: "openai-image"},
		{name: "image entry stream", entry: " OPENAI-IMAGE ", stream: true},
		{name: "image exit stream", entry: "openai", exit: "openai-image", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			captured := make(chan coreexecutor.Options, 1)
			executor := &lifecycleCaptureExecutor{
				provider: "codex",
				execute: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
					captured <- opts
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				},
				stream: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					captured <- opts
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newLifecycleHandler(t, "gpt-image-1.5", executor)
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			ctx := util.WithGinContext(context.Background(), ginCtx)
			req := ModelExecutionRequest{
				EntryProtocol: test.entry, ExitProtocol: test.exit,
				Model: "gpt-image-1.5", Stream: test.stream,
				Body: []byte(`{"model":"gpt-image-1.5"}`), Path: " /v1/images/edits ",
			}
			if test.stream {
				response, err := handler.ExecuteModelStream(ctx, req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("stream response = %#v, error = %#v", response, err)
				}
				for range response.Chunks {
				}
			} else {
				response, err := handler.ExecuteModel(ctx, req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("response = %#v, error = %#v", response, err)
				}
			}
			select {
			case opts := <-captured:
				if got := opts.Metadata[coreexecutor.RequestPathMetadataKey]; got != "/v1/images/edits" {
					t.Fatalf("request path = %#v", got)
				}
			default:
				t.Fatal("model executor was not invoked")
			}
		})
	}
}

func TestInternalModelExecutionDoesNotInheritTransportAcknowledgement(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			seenContext := make(chan context.Context, 1)
			executor := &lifecycleCaptureExecutor{
				provider: "codex",
				execute: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
					seenContext <- ctx
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				},
				stream: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					seenContext <- ctx
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newLifecycleHandler(t, "nested-ack-test", executor)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := coreexecutor.WithStreamDeliveryAcknowledgement(parent)
			req := ModelExecutionRequest{EntryProtocol: "openai", Model: "nested-ack-test", Stream: stream}
			if stream {
				response, err := handler.ExecuteModelStream(ctx, req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("stream response = %#v, error = %#v", response, err)
				}
				for range response.Chunks {
				}
			} else {
				response, err := handler.ExecuteModel(ctx, req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("response = %#v, error = %#v", response, err)
				}
			}
			nested := <-seenContext
			if coreexecutor.StreamDeliveryAcknowledgement(nested) {
				t.Fatal("nested execution inherited the parent transport acknowledgement")
			}
			if !coreexecutor.StreamDeliveryAcknowledgement(ctx) {
				t.Fatal("parent transport acknowledgement was changed")
			}
			cancel()
			if nested.Err() != context.Canceled {
				t.Fatal("nested execution lost parent cancellation")
			}
		})
	}
}

func TestRequestInterceptorsOverrideExecutionPath(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			var afterAuthPath string
			captured := make(chan string, 1)
			executor := &lifecycleCaptureExecutor{
				provider: "codex",
				execute: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
					captured <- opts.Metadata[coreexecutor.RequestPathMetadataKey].(string)
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				},
				stream: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					captured <- opts.Metadata[coreexecutor.RequestPathMetadataKey].(string)
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newLifecycleHandler(t, "model-path-override", executor)
			handler.PluginHost = &lifecycleTestHost{
				interceptRequestBeforeAuth: func(_ context.Context, _ pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
					return pluginapi.RequestInterceptResponse{Path: "/v1/images/generations"}
				},
				interceptRequestAfterAuth: func(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
					afterAuthPath, _ = req.Metadata[coreexecutor.RequestPathMetadataKey].(string)
					return pluginapi.RequestInterceptResponse{Path: " /v1/images/edits "}
				},
			}
			req := ModelExecutionRequest{
				EntryProtocol: "openai", Model: "model-path-override", Stream: stream,
				Body: []byte(`{"model":"model-path-override"}`), Path: "/v1/chat/completions",
			}
			if stream {
				response, err := handler.ExecuteModelStream(context.Background(), req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("stream response = %#v, error = %#v", response, err)
				}
				for range response.Chunks {
				}
			} else {
				response, err := handler.ExecuteModel(context.Background(), req)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("response = %#v, error = %#v", response, err)
				}
			}
			if afterAuthPath != "/v1/images/generations" {
				t.Fatalf("path after pre-auth interceptor = %q", afterAuthPath)
			}
			select {
			case got := <-captured:
				if got != "/v1/images/edits" {
					t.Fatalf("executor path = %q", got)
				}
			default:
				t.Fatal("model executor was not invoked")
			}
		})
	}
}

func TestInvalidInternalModelExecutionPathRejectsBeforeDispatch(t *testing.T) {
	for _, path := range []string{"https://example.com/v1/images/edits", "//example.com/v1/images/edits", "/v1/images/edits?key=value", "/v1/images\\edits", "/v1/images/%ZZ"} {
		t.Run(path, func(t *testing.T) {
			handler := NewBaseAPIHandlers(nil, nil)
			req := ModelExecutionRequest{EntryProtocol: "openai", Model: "model", Path: path}
			if _, err := handler.ExecuteModel(context.Background(), req); err == nil || err.StatusCode != http.StatusBadRequest {
				t.Fatalf("non-stream path %q returned %#v", path, err)
			}
			req.Stream = true
			if _, err := handler.ExecuteModelStream(context.Background(), req); err == nil || err.StatusCode != http.StatusBadRequest {
				t.Fatalf("stream path %q returned %#v", path, err)
			}
		})
	}
}

func TestInvalidInterceptorPathRejectsBeforeDispatch(t *testing.T) {
	for _, stage := range []string{"before auth", "after auth"} {
		for _, stream := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: " non-stream", true: " stream"}[stream], func(t *testing.T) {
				invoked := false
				executor := &lifecycleCaptureExecutor{
					provider: "codex",
					execute: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
						invoked = true
						return coreexecutor.Response{}, nil
					},
					stream: func(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
						invoked = true
						return nil, nil
					},
				}
				handler := newLifecycleHandler(t, "invalid-interceptor-path", executor)
				invalidPath := func(_ context.Context, _ pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
					return pluginapi.RequestInterceptResponse{Path: "https://invalid.example/v1/images/edits"}
				}
				host := &lifecycleTestHost{}
				if stage == "before auth" {
					host.interceptRequestBeforeAuth = invalidPath
				} else {
					host.interceptRequestAfterAuth = invalidPath
				}
				handler.PluginHost = host
				req := ModelExecutionRequest{EntryProtocol: "openai", Model: "invalid-interceptor-path", Stream: stream}
				if stream {
					if _, err := handler.ExecuteModelStream(context.Background(), req); err == nil || err.StatusCode != http.StatusBadRequest {
						t.Fatalf("stream invalid interceptor path returned %#v", err)
					}
				} else if _, err := handler.ExecuteModel(context.Background(), req); err == nil || err.StatusCode != http.StatusBadRequest {
					t.Fatalf("non-stream invalid interceptor path returned %#v", err)
				}
				if invoked {
					t.Fatal("model executor invoked after invalid request path")
				}
			})
		}
	}
}

func TestRequestPathOverrideDoesNotMutateExistingMetadata(t *testing.T) {
	metadata := map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"}
	opts := coreexecutor.Options{Metadata: metadata}
	if !overrideRequestPathMetadata(&opts, "/v1/images/generations") {
		t.Fatal("valid request path was rejected")
	}
	if got := metadata[coreexecutor.RequestPathMetadataKey]; got != "/v1/chat/completions" {
		t.Fatalf("original metadata path = %#v", got)
	}
	if got := opts.Metadata[coreexecutor.RequestPathMetadataKey]; got != "/v1/images/generations" {
		t.Fatalf("overridden metadata path = %#v", got)
	}
	if overrideRequestPathMetadata(&opts, "https://example.com/v1/images/edits") {
		t.Fatal("invalid interceptor path was accepted")
	}
}
