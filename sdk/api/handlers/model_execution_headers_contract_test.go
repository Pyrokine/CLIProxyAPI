package handlers

import (
	"context"
	"fmt"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"net/http"
	"testing"
)

func TestExecuteModelReturnsFilteredHeadersWhenPassthroughDisabled(t *testing.T) {
	model := "model-execution-internal-headers"
	requestBody := []byte(fmt.Sprintf(`{"model":%q}`, model))
	executor := &lifecycleCaptureExecutor{
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{
				Payload: []byte(`{"ok":true}`),
				Headers: http.Header{
					"X-Ratelimit-Remaining": []string{"7"},
					"Set-Cookie":            []string{"secret=1"},
				},
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)

	resp, errMsg := handler.ExecuteModel(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         model,
		Body:          requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModel() error = %+v", errMsg)
	}
	if resp.Headers.Get("X-Ratelimit-Remaining") != "7" {
		t.Fatalf("headers = %#v, want filtered upstream quota header", resp.Headers)
	}
	if resp.Headers.Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie = %q, want filtered", resp.Headers.Get("Set-Cookie"))
	}

	handler.SetPluginHost(&lifecycleTestHost{})
	resp, errMsg = handler.ExecuteModel(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         model,
		Body:          requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModel() with interceptor error = %+v", errMsg)
	}
	if resp.Headers.Get("X-Ratelimit-Remaining") != "7" || resp.Headers.Get("Set-Cookie") != "" {
		t.Fatalf("intercepted internal headers = %#v", resp.Headers)
	}

	_, clientHeaders, clientErr := handler.ExecuteWithAuthManager(context.Background(), "openai", model, requestBody, "")
	if clientErr != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", clientErr)
	}
	if clientHeaders.Get("X-Ratelimit-Remaining") != "" {
		t.Fatalf("client headers = %#v, want passthrough disabled", clientHeaders)
	}
}

func TestExecuteModelStreamReturnsFilteredHeadersWhenPassthroughDisabled(t *testing.T) {
	model := "model-execution-internal-stream-headers"
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model))
	executor := &lifecycleCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{
					"X-Ratelimit-Remaining": []string{"3"},
					"Set-Cookie":            []string{"secret=1"},
				},
				Chunks: chunks,
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         model,
		Stream:        true,
		Body:          requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModelStream() error = %+v", errMsg)
	}
	if stream.Headers.Get("X-Ratelimit-Remaining") != "3" {
		t.Fatalf("headers = %#v, want filtered upstream quota header", stream.Headers)
	}
	if stream.Headers.Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie = %q, want filtered", stream.Headers.Get("Set-Cookie"))
	}

	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	data, publicHeaders, errs := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, requestBody, "")
	for range data {
	}
	for errMsg := range errs {
		if errMsg != nil {
			t.Fatal(errMsg)
		}
	}
	if publicHeaders.Get("X-Ratelimit-Remaining") != "" {
		t.Fatalf("public stream leaked upstream headers: %#v", publicHeaders)
	}
}
