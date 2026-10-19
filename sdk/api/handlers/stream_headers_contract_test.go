package handlers

import (
	"context"
	"fmt"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"net/http"
	"testing"
)

func TestHandlerStreamInterceptorInitializesHeadersBeforeReturn(t *testing.T) {
	model := "handler-interceptor-stream-header-before-return-model"
	initStarted := make(chan struct{})
	allowInit := make(chan struct{})
	executor := &lifecycleCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("payload")}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{"X-Upstream": []string{"stream"}},
				Chunks:  chunks,
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(&sdkconfig.SDKConfig{PassthroughHeaders: true})
	handler.SetPluginHost(&lifecycleTestHost{
		interceptStreamChunk: func(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
			headers := cloneHeader(req.ResponseHeaders)
			if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
				close(initStarted)
				<-allowInit
				headers.Set("X-Init", "plugin")
			}
			return pluginapi.StreamChunkInterceptResponse{
				Headers: headers,
				Body:    cloneBytes(req.Body),
			}
		},
	})

	type streamResult struct {
		dataChan        <-chan []byte
		upstreamHeaders http.Header
		errChan         <-chan *ErrorMessage
	}
	resultChan := make(chan streamResult, 1)
	go func() {
		dataChan, upstreamHeaders, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
		resultChan <- streamResult{dataChan: dataChan, upstreamHeaders: upstreamHeaders, errChan: errChan}
	}()

	select {
	case result := <-resultChan:
		t.Fatalf("ExecuteStreamWithAuthManager returned before stream header init: %#v", result.upstreamHeaders)
	case <-initStarted:
	}
	select {
	case result := <-resultChan:
		t.Fatalf("ExecuteStreamWithAuthManager returned while stream header init was blocked: %#v", result.upstreamHeaders)
	default:
	}
	close(allowInit)

	result := <-resultChan
	dataChan := result.dataChan
	upstreamHeaders := result.upstreamHeaders
	errChan := result.errChan
	if upstreamHeaders.Get("X-Init") != "plugin" {
		t.Fatalf("upstream headers before first payload = %#v, want initialized plugin header", upstreamHeaders)
	}
	for range dataChan {
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}
}

func TestHandlerStreamInterceptorKeepsReturnedHeadersStableAfterFirstPayload(t *testing.T) {
	model := "handler-interceptor-stream-stable-headers-model"
	releaseSecond := make(chan struct{})
	executor := &lifecycleCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk)
			go func() {
				defer close(chunks)
				chunks <- coreexecutor.StreamChunk{Payload: []byte("first")}
				<-releaseSecond
				chunks <- coreexecutor.StreamChunk{Payload: []byte("second")}
			}()
			return &coreexecutor.StreamResult{
				Headers: http.Header{"X-Upstream": []string{"stream"}},
				Chunks:  chunks,
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(&sdkconfig.SDKConfig{PassthroughHeaders: true})
	handler.SetPluginHost(&lifecycleTestHost{
		interceptStreamChunk: func(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
			headers := cloneHeader(req.ResponseHeaders)
			switch req.ChunkIndex {
			case pluginapi.StreamChunkHeaderInitIndex:
				headers.Set("X-Stage", "init")
			case 0:
				headers.Set("X-Chunk", "first")
			case 1:
				headers.Set("X-Chunk", "second")
			}
			return pluginapi.StreamChunkInterceptResponse{
				Headers: headers,
				Body:    cloneBytes(req.Body),
			}
		},
	})

	dataChan, upstreamHeaders, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	firstChunk, ok := <-dataChan
	if !ok {
		t.Fatal("data channel closed before first chunk")
	}
	if string(firstChunk) != "first" {
		t.Fatalf("first chunk = %q, want first", firstChunk)
	}
	if upstreamHeaders.Get("X-Chunk") != "first" || upstreamHeaders.Get("X-Stage") != "init" {
		t.Fatalf("upstream headers after first chunk = %#v, want first transformed chunk headers", upstreamHeaders)
	}

	close(releaseSecond)
	got := append([]byte(nil), firstChunk...)
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}
	if string(got) != "firstsecond" {
		t.Fatalf("stream payload = %q, want firstsecond", got)
	}
	if upstreamHeaders.Get("X-Chunk") != "first" {
		t.Fatalf("upstream headers changed after return: %#v", upstreamHeaders)
	}
}

func TestHandlerStreamInterceptorReturnedHeadersImmutableAfterReturn(t *testing.T) {
	model := "handler-interceptor-stream-immutable-headers-model"
	releaseSecond := make(chan struct{})
	bodyStarted := make(chan struct{})
	releaseBody := make(chan struct{})
	executor := &lifecycleCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk)
			go func() {
				defer close(chunks)
				chunks <- coreexecutor.StreamChunk{Payload: []byte("first")}
				<-releaseSecond
				chunks <- coreexecutor.StreamChunk{Payload: []byte("second")}
			}()
			return &coreexecutor.StreamResult{
				Headers: http.Header{"X-Upstream": []string{"stream"}},
				Chunks:  chunks,
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(&sdkconfig.SDKConfig{PassthroughHeaders: true})
	handler.SetPluginHost(&lifecycleTestHost{
		interceptStreamChunk: func(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
			headers := cloneHeader(req.ResponseHeaders)
			switch req.ChunkIndex {
			case pluginapi.StreamChunkHeaderInitIndex:
				headers.Set("X-Init", "plugin")
			case 1:
				close(bodyStarted)
				<-releaseBody
				headers.Set("X-Body", "plugin")
			}
			return pluginapi.StreamChunkInterceptResponse{Headers: headers, Body: cloneBytes(req.Body)}
		},
	})

	dataChan, upstreamHeaders, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	dataDone := make(chan struct{})
	go func() {
		defer close(dataDone)
		for range dataChan {
		}
	}()
	stopReading := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stopReading:
				return
			default:
				_ = upstreamHeaders.Get("X-Init")
			}
		}
	}()

	close(releaseSecond)
	<-bodyStarted
	close(releaseBody)
	<-dataDone
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}
	close(stopReading)
	<-readerDone
	if upstreamHeaders.Get("X-Init") != "plugin" || upstreamHeaders.Get("X-Body") != "" {
		t.Fatalf("returned headers mutated after return: %#v", upstreamHeaders)
	}
}

func TestHandlerStreamInterceptorInitializesHeadersWithoutPayload(t *testing.T) {
	model := "handler-interceptor-stream-header-only-model"
	executor := &lifecycleCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("payload")}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{"X-Upstream": []string{"stream"}},
				Chunks:  chunks,
			}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	handler.UpdateClients(&sdkconfig.SDKConfig{PassthroughHeaders: true})
	var initCalls int
	var payloadCalls int
	handler.SetPluginHost(&lifecycleTestHost{
		interceptStreamChunk: func(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
			if req.ChunkIndex != pluginapi.StreamChunkHeaderInitIndex {
				payloadCalls++
				if string(req.Body) != "payload" || req.ResponseHeaders.Get("X-Init") != "plugin" {
					t.Fatalf("payload stream request = %#v, want initialized headers and payload", req)
				}
				return pluginapi.StreamChunkInterceptResponse{Headers: cloneHeader(req.ResponseHeaders), Body: cloneBytes(req.Body)}
			}
			initCalls++
			headers := cloneHeader(req.ResponseHeaders)
			headers.Set("X-Init", "plugin")
			return pluginapi.StreamChunkInterceptResponse{Headers: headers}
		},
	})

	dataChan, upstreamHeaders, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	for chunk := range dataChan {
		if string(chunk) != "payload" {
			t.Fatalf("stream chunk = %q, want payload", chunk)
		}
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}
	if initCalls != 1 {
		t.Fatalf("initial stream calls = %d, want 1", initCalls)
	}
	if payloadCalls != 1 {
		t.Fatalf("payload stream calls = %d, want 1", payloadCalls)
	}
	if upstreamHeaders.Get("X-Init") != "plugin" {
		t.Fatalf("upstream headers = %#v, want initial plugin header", upstreamHeaders)
	}
}
