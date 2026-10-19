package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiInteractionsRejectsUnsendableAttachmentBeforeDispatch(t *testing.T) {
	calls := 0
	ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected upstream request")
	}))
	exec := NewGeminiInteractionsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test-key"}}
	req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = exec.ExecuteStream(ctx, auth, req, opts)
		} else {
			_, err = exec.Execute(ctx, auth, req, opts)
		}
		var rejected *translatorcommon.UnsupportedPartError
		if !errors.As(err, &rejected) || rejected.StatusCode() != http.StatusBadRequest || !rejected.IsRequestScoped() {
			t.Fatalf("stream=%t error=%v, want request-scoped 400", stream, err)
		}
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want zero", calls)
	}
}

func TestGeminiInteractionsExecutorSanitizesInputStepIDs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var upstreamBody []byte
			ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				var err error
				upstreamBody, err = io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request: %v", err)
				}
				response := `{"interaction":{"steps":[]}}`
				if stream {
					response = "data: [DONE]\n\n"
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(response)),
					Request:    req,
				}, nil
			}))
			exec := NewGeminiInteractionsExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test-key"}}
			request := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":[
				{"type":"user_input","id":"remove-step","content":[{"type":"text","id":"remove-part","text":"hi"}]},
				{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup","arguments":{}},
				{"type":"function_result","id":"remove-result","call_id":"call-1","name":"lookup","result":"ok"}
			]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions}
			if stream {
				result, err := exec.ExecuteStream(ctx, auth, request, opts)
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				for range result.Chunks {
				}
			} else if _, err := exec.Execute(ctx, auth, request, opts); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			inputs := gjson.GetBytes(upstreamBody, "input").Array()
			if len(inputs) != 3 {
				t.Fatalf("input count = %d, request = %s", len(inputs), upstreamBody)
			}
			if inputs[0].Get("id").Exists() || inputs[0].Get("content.0.id").Exists() ||
				inputs[1].Get("id").String() != "call-1" || inputs[1].Get("call_id").Exists() ||
				inputs[2].Get("id").Exists() || inputs[2].Get("call_id").String() != "call-1" {
				t.Fatalf("unsupported input IDs remain: %s", upstreamBody)
			}
		})
	}
}
