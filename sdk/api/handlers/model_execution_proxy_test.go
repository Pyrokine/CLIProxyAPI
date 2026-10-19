package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestExecuteModelRequestProxyReachesExecutorAndClearsInheritedOverride(t *testing.T) {
	const model = "request-proxy-model"
	var captured []string
	provider := &lifecycleCaptureExecutor{
		provider: "openai",
		execute: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			captured = append(captured, opts.ProxyURL, coreexecutor.RequestProxyURL(ctx))
			return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
	handler := newLifecycleHandler(t, model, provider)
	ctx := coreexecutor.WithRequestProxyURL(context.Background(), "http://inherited.example:8080")
	for _, reqProxy := range []string{"http://127.0.0.1:8080", ""} {
		response, err := handler.ExecuteModel(ctx, ModelExecutionRequest{
			EntryProtocol: constant.OpenAI, Model: model, ProxyURL: reqProxy,
			Body: []byte(`{"model":"request-proxy-model"}`),
		})
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("ExecuteModel proxy=%q response=%#v error=%#v", reqProxy, response, err)
		}
		if len(captured) != 2 || captured[0] != reqProxy || captured[1] != reqProxy {
			t.Fatalf("proxy=%q observed=%v", reqProxy, captured)
		}
		captured = nil
	}
}

func TestExecuteModelStreamRequestProxyReachesExecutor(t *testing.T) {
	const model = "request-proxy-stream-model"
	var captured []string
	provider := &lifecycleCaptureExecutor{
		provider: "openai",
		stream: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			captured = append(captured, opts.ProxyURL, coreexecutor.RequestProxyURL(ctx))
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\\n\\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newLifecycleHandler(t, model, provider)
	stream, err := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: constant.OpenAI, Model: model, Stream: true,
		ProxyURL: "socks5://127.0.0.1:1080", Body: []byte(`{"model":"request-proxy-stream-model"}`),
	})
	if err != nil || stream.StatusCode != http.StatusOK {
		t.Fatalf("ExecuteModelStream result=%#v error=%#v", stream, err)
	}
	for range stream.Chunks {
	}
	if len(captured) != 2 || captured[0] != "socks5://127.0.0.1:1080" || captured[1] != captured[0] {
		t.Fatalf("stream proxy observed=%v", captured)
	}
}

func TestExecuteModelRejectsInvalidRequestProxyBeforeDispatch(t *testing.T) {
	for _, proxyURL := range []string{"direct", "http://localhost:0", "socks5://localhost:65536", "https://"} {
		for _, stream := range []bool{false, true} {
			handler := &BaseAPIHandler{}
			req := ModelExecutionRequest{Stream: stream, ProxyURL: proxyURL}
			if stream {
				_, err := handler.ExecuteModelStream(context.Background(), req)
				if err == nil || err.StatusCode != http.StatusBadRequest {
					t.Fatalf("invalid stream proxy=%q error=%#v", proxyURL, err)
				}
			} else {
				_, err := handler.ExecuteModel(context.Background(), req)
				if err == nil || err.StatusCode != http.StatusBadRequest {
					t.Fatalf("invalid proxy=%q error=%#v", proxyURL, err)
				}
			}
		}
	}
}
