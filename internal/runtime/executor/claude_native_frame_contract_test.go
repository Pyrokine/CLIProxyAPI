package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeNativeStreamDeliversCompleteEvents(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: " +
			(`{"type":"message_start","message":{"id":"fixture","model":` +
				`"claude-test","usage":{"input_tokens":` +
				`2}}}`),
		"event: content_block_start\ndata: " +
			(`{"type":"content_block_start","index":0,"content_block":` +
				`{"type":"text","text":` +
				`"fixture"}}`),
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: " +
			(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` +
				`{"output_tokens":` +
				`3}}`),
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
	}
	for _, fixture := range []struct {
		name, end string
		readError error
	}{
		{name: "terminated", end: "\n\n"},
		{name: "EOF after data"},
		{name: "EOF after newline", end: "\n"},
		{name: "error after terminal", end: "\n\n", readError: io.ErrUnexpectedEOF},
	} {
		t.Run(
			fixture.name, func(t *testing.T) {
				body := strings.Join(frames, "\n\n") + fixture.end
				transport := &auditClaudeContractTransport{
					respond: func(request *http.Request) (*http.Response, error) {
						if request.URL.Host != "claude-frames.synthetic.invalid" {
							t.Fatalf("unexpected upstream URL: %s", request.URL)
						}
						if _, err := io.ReadAll(request.Body); err != nil {
							return nil, err
						}
						var responseBody = io.NopCloser(strings.NewReader(body))
						if fixture.readError != nil {
							responseBody = io.NopCloser(
								&terminalErrorReader{
									data: []byte(body), err: fixture.readError,
								},
							)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
							Body: responseBody, Request: request,
						}, nil
					},
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = util.WithRoundTripper(ctx, transport)
				cfg := &config.Config{}
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: "claude", Attributes: map[string]string{
						"api_key": "synthetic-key", "base_url": "https://claude-frames.synthetic.invalid",
					},
				}
				if newProxyAwareHTTPClient(ctx, cfg, auth, 0).Transport != transport {
					t.Fatal("Claude transport is not memory-isolated")
				}
				result, err := NewClaudeExecutor(cfg).ExecuteStream(
					ctx, auth, core.Request{
						Model:   "claude-test",
						Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`),
					}, core.Options{SourceFormat: translator.FormatClaude, ResponseFormat: translator.FormatClaude},
				)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					if !bytes.HasSuffix(chunk.Payload, []byte("\n\n")) || bytes.Count(
						chunk.Payload, []byte("event:"),
					) != 1 || bytes.Count(chunk.Payload, []byte("data:")) != 1 {
						t.Errorf("native chunk is not one complete event: %q", chunk.Payload)
						continue
					}
					lines := strings.Split(string(chunk.Payload), "\n")
					event := strings.TrimPrefix(lines[0], "event: ")
					data := strings.TrimPrefix(lines[1], "data: ")
					if gjson.Get(data, "type").String() != event {
						t.Errorf("event header and payload differ: %q", chunk.Payload)
					}
					count++
				}
				if count != len(frames) {
					t.Fatalf("complete native events=%d, want %d", count, len(frames))
				}
			},
		)
	}
}
