package handlers

import (
	"context"
	"testing"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestRequestInterceptorsDoNotExposeOriginalPayloadToMutatingHost(t *testing.T) {
	host := &lifecycleTestHost{
		interceptRequestBeforeAuth: func(_ context.Context, request pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			request.Body[0] = 'X'
			return pluginapi.RequestInterceptResponse{}
		},
		interceptRequestAfterAuth: func(_ context.Context, request pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			request.Body[0] = 'X'
			return pluginapi.RequestInterceptResponse{}
		},
	}
	handler := &BaseAPIHandler{PluginHost: host}
	beforeBody := []byte("original")
	before, _, errMsg := handler.applyRequestInterceptorsBeforeAuth(
		context.Background(), "openai", "model", coreexecutor.Request{Payload: beforeBody}, coreexecutor.Options{}, nil, "",
	)
	if errMsg != nil || string(beforeBody) != "original" || string(before.Payload) != "original" {
		t.Fatalf("before-auth body = %q, request = %q, error = %v", beforeBody, before.Payload, errMsg)
	}

	afterBody := []byte("original")
	after := handler.applyRequestInterceptorsAfterAuth(
		context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
			Request: coreexecutor.Request{Payload: afterBody},
		}, nil, "",
	)
	if string(afterBody) != "original" || string(after.Request.Payload) != "original" {
		t.Fatalf("after-auth body = %q, request = %q", afterBody, after.Request.Payload)
	}
}
