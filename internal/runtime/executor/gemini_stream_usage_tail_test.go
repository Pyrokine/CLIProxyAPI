package executor

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/tidwall/gjson"
)

func geminiUsageTailFrames(wrapped bool) string {
	frames := []string{
		`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10}}`,
		`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2}}`,
		`{"usageMetadata":{"cachedContentTokenCount":4,"thoughtsTokenCount":3,"totalTokenCount":15}}`,
	}
	for i, frame := range frames {
		if wrapped {
			frame = `{"response":` + frame + `}`
		}
		frames[i] = "data: " + frame + "\n\n"
	}
	return strings.Join(frames, "")
}

func TestAntigravityAggregateRetainsSplitAndLateUsage(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude} {
		t.Run(format.String(), func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, plugin)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(geminiUsageTailFrames(true))), Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{ID: "aggregate-usage", Provider: "antigravity",
				Attributes: map[string]string{"base_url": "https://gemini.test"},
				Metadata: map[string]any{"access_token": "offline-fixture", "project_id": "fixture-project",
					"expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
			payload := []byte(`{"model":"claude-sonnet-4-6","input":"hi"}`)
			if format == sdktranslator.FormatClaude {
				payload = []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
			}
			response, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth,
				cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: payload})
			if err != nil {
				t.Fatal(err)
			}
			finalUsage := gjson.GetBytes(response.Payload, "usage")
			wantInput, cachePath := int64(10), "input_tokens_details.cached_tokens"
			if format == sdktranslator.FormatClaude {
				wantInput, cachePath = 6, "cache_read_input_tokens"
			}
			if finalUsage.Get("input_tokens").Int() != wantInput || finalUsage.Get("output_tokens").Int() != 5 ||
				finalUsage.Get(cachePath).Int() != 4 {
				t.Errorf("aggregated usage=%s", finalUsage.Raw)
			}
			select {
			case record := <-plugin.records:
				if record.Failed || record.Detail.InputTokens != 10 || record.Detail.OutputTokens != 2 ||
					record.Detail.ReasoningTokens != 3 || record.Detail.CachedTokens != 4 || record.Detail.TotalTokens != 15 {
					t.Errorf("recorded usage=%+v failed=%t", record.Detail, record.Failed)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for aggregate usage")
			}
		})
	}
}

func TestAntigravityAggregatePreservesRawMetadataAndParts(t *testing.T) {
	stream := strings.Join([]string{
		`{"traceId":"fixture-trace","response":{"modelVersion":"claude-sonnet-4-6","responseId":"fixture-response","extra":{"value":9007199254740993},"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"reason","thought_signature":"fixture-signature"},{"text":"answer"}]},"groundingMetadata":{"webSearchQueries":["fixture"]}}],"cpaUsageMetadata":{"promptTokenCount":10,"promptTokensDetails":[{"tokenCount":9007199254740993}]}}}`,
		`{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"lookup","args":{"value":9007199254740993}},"thought_signature":"tool-signature"},{"inline_data":{"mime_type":"image/png","data":"fixture-data"}}]}}],"usageMetadata":{"candidatesTokenCount":2}}}`,
		`{"response":{"usageMetadata":{"promptTokenCount":"invalid","thoughtsTokenCount":3,"cachedContentTokenCount":4,"totalTokenCount":15}}}`,
	}, "\n")
	payload, err := NewAntigravityExecutor(&config.Config{}).convertStreamToNonStream([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(payload)
	for path, want := range map[string]string{
		"traceId": "fixture-trace", "response.modelVersion": "claude-sonnet-4-6", "response.responseId": "fixture-response",
		"response.extra.value": "9007199254740993",
		"response.usageMetadata.promptTokensDetails.0.tokenCount":       "9007199254740993",
		"response.candidates.0.finishReason":                            "STOP",
		"response.candidates.0.groundingMetadata.webSearchQueries.0":    "fixture",
		"response.candidates.0.content.parts.0.thoughtSignature":        "fixture-signature",
		"response.candidates.0.content.parts.1.text":                    "answer",
		"response.candidates.0.content.parts.2.functionCall.args.value": "9007199254740993",
		"response.candidates.0.content.parts.2.thoughtSignature":        "tool-signature",
		"response.candidates.0.content.parts.3.inlineData.data":         "fixture-data",
	} {
		if got := root.Get(path).String(); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	for key, want := range map[string]int64{"promptTokenCount": 10, "candidatesTokenCount": 2,
		"thoughtsTokenCount": 3, "cachedContentTokenCount": 4, "totalTokenCount": 15} {
		if got := root.Get("response.usageMetadata." + key).Int(); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if root.Get("response.cpaUsageMetadata").Exists() {
		t.Error("internal usage alias leaked into aggregate response")
	}
	for _, invalid := range []string{`{`, `{"response":[]}`, `{"response":{"candidates":[42]}}`,
		`{"response":{"candidates":[{"content":{"parts":[null]}}]}}`} {
		if _, err := NewAntigravityExecutor(&config.Config{}).convertStreamToNonStream([]byte(invalid)); err == nil {
			t.Errorf("invalid aggregate %s returned success", invalid)
		}
	}
}

func TestAntigravityAggregatePreservesDistinctSignedParts(t *testing.T) {
	stream := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"first","thoughtSignature":"first-signature"},{"thought":true,"text":"second","thought_signature":"second-signature"},{"text":"visible","thoughtSignature":"visible-signature","extra":9007199254740993},{"text":" "}]},"finishReason":"STOP"}]}}`)
	payload, err := NewAntigravityExecutor(&config.Config{}).convertStreamToNonStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(payload, "response.candidates.0.content.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("signed parts changed: %s", payload)
	}
	for i, want := range []string{"first-signature", "second-signature", "visible-signature"} {
		if got := parts[i].Get("thoughtSignature").String(); got != want {
			t.Errorf("part %d signature=%s, want %s", i, got, want)
		}
	}
	if parts[2].Get("extra").Raw != "9007199254740993" || parts[3].Get("text").String() != " " {
		t.Errorf("part metadata or whitespace changed: %s", payload)
	}
}

func TestAntigravityAggregateReportsLateFailuresWithUsage(t *testing.T) {
	for _, failure := range []string{"read", "upstream", "truncated", "missing-terminal", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, plugin)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			frames := geminiUsageTailFrames(true)
			var body io.ReadCloser = io.NopCloser(strings.NewReader(frames))
			wantOutput, wantReasoning, wantCached := int64(2), int64(3), int64(4)
			switch failure {
			case "read":
				body = &errorAfterFirstLineBody{reader: bytes.NewReader([]byte(frames)), err: errors.New("fixture read failure")}
			case "upstream":
				body = io.NopCloser(strings.NewReader(frames + "data: {\"error\":{\"code\":500,\"message\":\"fixture failure\"}}\n\n"))
			case "truncated":
				body = io.NopCloser(strings.NewReader(frames + "data: {\"response\":"))
			case "missing-terminal":
				body = io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]}}],\"usageMetadata\":{\"promptTokenCount\":10}}}\n\n"))
				wantOutput, wantReasoning, wantCached = 0, 0, 0
			case "canceled":
				blocked := &cancelThenEOFBody{reader: strings.NewReader(frames), ctx: ctx, started: make(chan struct{})}
				body = blocked
				go func(ctx context.Context) {
					select {
					case <-blocked.started:
						cancel()
					case <-ctx.Done():
					}
				}(ctx)
			}
			ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: body, Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{ID: "aggregate-failure", Provider: "antigravity",
				Attributes: map[string]string{"base_url": "https://gemini.test"},
				Metadata: map[string]any{"access_token": "offline-fixture", "project_id": "fixture-project",
					"expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
			payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
			response, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth,
				cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload})
			if err == nil || len(response.Payload) != 0 {
				t.Fatalf("aggregate failure returned response=%s error=%v", response.Payload, err)
			}
			select {
			case record := <-plugin.records:
				if !record.Failed || record.Detail.InputTokens != 10 || record.Detail.OutputTokens != wantOutput ||
					record.Detail.ReasoningTokens != wantReasoning || record.Detail.CachedTokens != wantCached {
					t.Errorf("recorded usage=%+v failed=%t", record.Detail, record.Failed)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for aggregate failure usage")
			}
		})
	}
}

func TestGeminiFamilyStreamRetainsSplitAndLateUsage(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex", "antigravity"} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude} {
			t.Run(provider+"/"+format.String(), func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
				registerTestUsagePlugin(t, plugin)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ctx = usage.WithRequestedModelAlias(ctx, "split-usage-"+provider)
				ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK,
						Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body:   io.NopCloser(strings.NewReader(geminiUsageTailFrames(provider == "antigravity"))), Request: req}, nil
				}))
				auth := &cliproxyauth.Auth{ID: "split-usage-" + provider, Provider: provider,
					Attributes: map[string]string{"api_key": "offline-fixture", "base_url": "https://gemini.test"},
					Metadata: map[string]any{"access_token": "offline-fixture", "project_id": "fixture-project",
						"expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
				payload := []byte(`{"model":"gemini-2.5-pro","input":"hi","stream":true}`)
				if format == sdktranslator.FormatClaude {
					payload = []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`)
				}
				req := cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: payload, Stream: true}
				var stream *cliproxyexecutor.StreamResult
				var err error
				switch provider {
				case "gemini":
					stream, err = NewGeminiExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
				case "vertex":
					stream, err = NewGeminiVertexExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
				case "antigravity":
					stream, err = NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				var finalUsage gjson.Result
				terminals := 0
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					for _, line := range strings.Split(string(chunk.Payload), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						data := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
						switch data.Get("type").String() {
						case "response.completed":
							terminals++
							finalUsage = data.Get("response.usage")
						case "message_delta":
							finalUsage = data.Get("usage")
						case "message_stop":
							terminals++
						}
					}
				}
				wantInput, wantCache := int64(10), "input_tokens_details.cached_tokens"
				if format == sdktranslator.FormatClaude {
					wantInput, wantCache = 6, "cache_read_input_tokens"
				}
				if terminals != 1 || finalUsage.Get("input_tokens").Int() != wantInput ||
					finalUsage.Get("output_tokens").Int() != 5 || finalUsage.Get(wantCache).Int() != 4 {
					t.Errorf("terminal count=%d usage=%s", terminals, finalUsage.Raw)
				}
				select {
				case record := <-plugin.records:
					if record.Failed || record.Detail.InputTokens != 10 || record.Detail.OutputTokens != 2 ||
						record.Detail.ReasoningTokens != 3 || record.Detail.CachedTokens != 4 || record.Detail.TotalTokens != 15 {
						t.Errorf("recorded usage=%+v failed=%t", record.Detail, record.Failed)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for recorded usage")
				}
			})
		}
	}
}
