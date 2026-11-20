package handlers

import (
	"context"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type modelExecutionSkipHost struct {
	lifecycleTestHost
	stages chan string
}

func (h *modelExecutionSkipHost) InterceptRequestBeforeAuthExcept(
	_ context.Context, req pluginapi.RequestInterceptRequest, skip string,
) pluginapi.RequestInterceptResponse {
	h.stages <- "before:" + skip
	return pluginapi.RequestInterceptResponse{Body: req.Body}
}

func (h *modelExecutionSkipHost) InterceptRequestAfterAuthExcept(
	_ context.Context, req pluginapi.RequestInterceptRequest, skip string,
) pluginapi.RequestInterceptResponse {
	h.stages <- "after:" + skip
	return pluginapi.RequestInterceptResponse{Body: req.Body}
}

func (h *modelExecutionSkipHost) InterceptResponseExcept(
	_ context.Context, req pluginapi.ResponseInterceptRequest, skip string,
) pluginapi.ResponseInterceptResponse {
	h.stages <- "response:" + skip
	return pluginapi.ResponseInterceptResponse{Body: req.Body}
}

func (h *modelExecutionSkipHost) InterceptStreamChunkExcept(
	_ context.Context, req pluginapi.StreamChunkInterceptRequest, skip string,
) pluginapi.StreamChunkInterceptResponse {
	h.stages <- "stream:" + skip
	return pluginapi.StreamChunkInterceptResponse{Body: req.Body}
}

func TestInternalModelExecutionSkipsCallerAtEachStage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct {
			name        string
			interceptor string
			router      string
			want        string
		}{
			{"canonical", "origin-plugin", "", "origin-plugin"},
			{"legacy", "", "legacy-origin", "legacy-origin"},
			{"canonical takes precedence", "origin-plugin", "legacy-origin", "origin-plugin"},
		} {
			name := "nonstream/" + tt.name
			if stream {
				name = "stream/" + tt.name
			}
			t.Run(
				name, func(t *testing.T) {
					const model = "nested-execution-stage-model"
					executor := &lifecycleCaptureExecutor{
						stream: func(
							context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options,
						) (*coreexecutor.StreamResult, error) {
							chunks := make(chan coreexecutor.StreamChunk, 1)
							chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
							close(chunks)
							return &coreexecutor.StreamResult{Chunks: chunks}, nil
						},
					}
					handler := newLifecycleHandler(t, model, executor)
					host := &modelExecutionSkipHost{stages: make(chan string, 16)}
					handler.PluginHost = host
					request := ModelExecutionRequest{
						EntryProtocol: "openai", Model: model, Stream: stream,
						Body:                    []byte(`{"model":"nested-execution-stage-model","messages":[]}`),
						SkipInterceptorPluginID: tt.interceptor,
						SkipRouterPluginID:      tt.router,
					}
					responseStage := "response"
					if stream {
						responseStage = "stream"
						response, err := handler.ExecuteModelStream(t.Context(), request)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := handler.ExecuteModel(t.Context(), request); err != nil {
						t.Fatal(err)
					}
					seen := make(map[string]int)
					for len(host.stages) > 0 {
						stage, skip, _ := strings.Cut(<-host.stages, ":")
						if skip != tt.want {
							t.Fatalf("%s skipped %q, want %q", stage, skip, tt.want)
						}
						seen[stage]++
					}
					if seen["before"] != 1 || seen["after"] != 1 || seen[responseStage] == 0 || len(seen) != 3 {
						t.Fatalf("nested execution stages=%v, want before/after/%s", seen, responseStage)
					}
					if captured := executor.captured(); captured.Model != model {
						t.Fatalf("nested execution changed requested model: %q", captured.Model)
					}
				},
			)
		}
	}
}
