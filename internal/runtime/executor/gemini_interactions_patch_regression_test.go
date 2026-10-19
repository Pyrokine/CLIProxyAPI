package executor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestGeminiInteractionsPatchSourceFailurePropagation(t *testing.T) {
	const start = `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}}`
	const stop = `{"event_type":"step.stop","index":0}`
	const terminal = `{"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"completed"}}`
	const request = `{"model":"gemini-2.5-pro","input":"synthetic prompt","tools":[{"type":"custom","name":"apply_patch"}]}`
	for _, tc := range []struct {
		name      string
		stream    bool
		frames    []string
		body      string
		noDone    bool
		wantError bool
	}{
		{name: "stream success", stream: true, frames: []string{start, stop, terminal}},
		{name: "source EOF", stream: true, frames: []string{start, stop}, noDone: true, wantError: true},
		{name: "malformed source JSON", stream: true, frames: []string{start, `{"event_type":"step.stop","index":0`, terminal}, wantError: true},
		{name: "source stop identity conflict", stream: true, frames: []string{start, `{"event_type":"step.stop","index":0,"step":{"id":"conflicting"}}`, terminal}, wantError: true},
		{name: "source stop barrier", stream: true, frames: []string{start, stop, `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":" "}}`, terminal}, wantError: true},
		{name: "upstream failure", stream: true, frames: []string{start, `{"event_type":"interaction.failed","error":{"message":"synthetic failure"}}`}, wantError: true},
		{name: "nonstream success", body: `{"id":"interaction-1","status":"completed","steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}]}`},
		{name: "nonstream malformed source", body: `{"id":"interaction-1","status":"completed","steps":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":{"input":"pq"}}]`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			path := ""
			body := tc.body
			if tc.stream {
				for _, frame := range tc.frames {
					body += "data: " + frame + "\n\n"
				}
				if !tc.noDone {
					body += "event: done\ndata: [DONE]\n\n"
				}
			}
			ctx := util.WithRoundTripper(t.Context(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				path = req.URL.Path
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://gemini-patch.invalid"}}
			exec := NewGeminiExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(request)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: []byte(request), Stream: tc.stream}
			var err error
			var output strings.Builder
			if tc.stream {
				var result *cliproxyexecutor.StreamResult
				result, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
						output.Write(chunk.Payload)
					}
				}
			} else {
				var result cliproxyexecutor.Response
				result, err = exec.Execute(ctx, auth, req, opts)
				output.Write(result.Payload)
			}
			if calls != 1 || !strings.HasSuffix(path, "/interactions") {
				t.Fatalf("transport calls = %d path = %q, want one Interactions request", calls, path)
			}
			if tc.wantError {
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != http.StatusBadGateway || strings.Contains(output.String(), `"type":"response.completed"`) {
					t.Fatalf("source failure was not propagated: error=%v output=%s", err, output.String())
				}
			} else if err != nil || !strings.Contains(output.String(), `"type":"custom_tool_call"`) || !strings.Contains(output.String(), `"input":"pq"`) {
				t.Fatalf("source success changed: error=%v output=%s", err, output.String())
			}
		})
	}
}

func TestGeminiInteractionsFailedSourceResponses(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
		for _, patch := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/patch=%t/failed=%t", format.String(), patch, failed), func(t *testing.T) {
					ctx := usage.WithRequestedModelAlias(t.Context(), t.Name())
					calls := 0
					body := `{"id":"synthetic-interaction","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}],"usage":{"total_input_tokens":5,"total_output_tokens":3,"total_tokens":8}}`
					if failed {
						body = `{"id":"synthetic-interaction","status":"failed","error":{"message":"RAW_SECRET"},"usage":{"total_input_tokens":5,"total_output_tokens":3,"total_tokens":8}}`
					}
					ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
						calls++
						if !strings.HasSuffix(req.URL.Path, "/interactions") {
							return nil, fmt.Errorf("unexpected endpoint: %s", req.URL.Path)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
					}))
					payload := []byte(`{"input":"synthetic prompt"}`)
					source := sdktranslator.FormatInteractions
					if patch {
						source = sdktranslator.FormatOpenAIResponse
						payload = []byte(`{"input":"synthetic prompt","tools":[{"type":"custom","name":"apply_patch"}]}`)
					}
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://interaction-failure.invalid"}}
					exec := NewGeminiInteractionsExecutor(&config.Config{})
					response, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: payload}, cliproxyexecutor.Options{SourceFormat: source, ResponseFormat: format, OriginalRequest: payload})
					if calls != 1 {
						t.Errorf("transport calls=%d, want one", calls)
					}
					if failed {
						var status interface{ StatusCode() int }
						if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway || len(response.Payload) != 0 || strings.Contains(err.Error(), "RAW_SECRET") {
							t.Errorf("source failure error=%v response=%s, want clean SDK 502 and empty result", err, response.Payload)
						}
					} else if err != nil || len(response.Payload) == 0 {
						t.Errorf("source success error=%v response=%s", err, response.Payload)
					}
					select {
					case record := <-plugin.records:
						if record.Alias != t.Name() || record.Failed != failed || record.Completed == failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 {
							t.Errorf("usage failed=%t completed=%t tokens=%d/%d/%d alias=%q, want failed=%t and 5/3/8", record.Failed, record.Completed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens, record.Alias, failed)
						}
					case <-time.After(time.Second):
						t.Error("usage record missing")
					}
					select {
					case record := <-plugin.records:
						t.Errorf("duplicate usage record: failed=%t", record.Failed)
					case <-time.After(20 * time.Millisecond):
					}
				})
			}
		}
	}
}
