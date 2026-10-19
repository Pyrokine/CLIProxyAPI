package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func syntheticAntigravityCAQS() string {
	appendVarint := func(dst []byte, field protowire.Number, value uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(dst, field, protowire.VarintType), value)
	}
	appendBytes := func(dst []byte, field protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(dst, field, protowire.BytesType), value)
	}
	channel := appendVarint(nil, 1, 18)
	channel = appendVarint(channel, 2, 2)
	channel = appendBytes(channel, 8, []byte("thinking"))
	container := appendBytes(nil, 1, channel)
	container = appendBytes(container, 5, make([]byte, 64))
	payload := appendVarint(nil, 1, 4)
	payload = appendBytes(payload, 2, container)
	return base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(payload)))
}

func TestAntigravityCAQSTwoTurnReplayWithLocalTransport(t *testing.T) {
	sig := syntheticAntigravityCAQS()
	for _, streaming := range []bool{false, true} {
		name := "nonstream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			internalcache.ClearAntigravityReasoningReplayCache()
			t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)
			const model = "claude-opus-5-5-high"
			response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"checked calculation","thoughtSignature":"` + sig + `"},{"text":"34113"}]},"finishReason":"STOP"}],"modelVersion":"` + model + `","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":15}}}`
			requests := make(chan []byte, 2)
			ctx := util.WithRoundTripper(context.Background(), antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				requests <- body
				contentType, payload := "application/json", response
				if strings.Contains(req.URL.Path, "streamGenerateContent") {
					contentType, payload = "text/event-stream", "data: "+response+"\n\n"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{
				ID: "caqs-two-turn-test", Attributes: map[string]string{"base_url": "https://antigravity.test"},
				Metadata: map[string]any{"access_token": "local-test-token", "project_id": "local-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
			}
			executor := NewAntigravityExecutor(&config.Config{})
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Stream: streaming}
			call := func(payload []byte) []byte {
				t.Helper()
				req := cliproxyexecutor.Request{Model: model, Payload: payload}
				if !streaming {
					res, err := executor.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					return res.Payload
				}
				res, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var out []byte
				for chunk := range res.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					out = append(out, chunk.Payload...)
				}
				return out
			}
			first := []byte(`{"model":"claude-opus-5-5-high","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"calculate"}]}`)
			downstream := call(first)
			<-requests
			var content []byte
			if streaming {
				var signatureOut, thinkingOut, textOut string
				for _, line := range bytes.Split(downstream, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data:")) {
						continue
					}
					event := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
					switch event.Get("delta.type").String() {
					case "signature_delta":
						signatureOut += event.Get("delta.signature").String()
					case "thinking_delta":
						thinkingOut += event.Get("delta.thinking").String()
					case "text_delta":
						textOut += event.Get("delta.text").String()
					}
				}
				if signatureOut != "claude#"+sig {
					t.Fatalf("stream changed synthetic Q signature provenance: %q", signatureOut)
				}
				content = []byte(`[ {"type":"thinking"}, {"type":"text"} ]`)
				content, _ = sjson.SetBytes(content, "0.thinking", thinkingOut)
				content, _ = sjson.SetBytes(content, "0.signature", signatureOut)
				content, _ = sjson.SetBytes(content, "1.text", textOut)
			} else {
				content = []byte(gjson.GetBytes(downstream, "content").Raw)
				if gjson.GetBytes(content, "0.signature").String() != "claude#"+sig {
					t.Fatalf("nonstream changed synthetic Q signature provenance: %s", content)
				}
			}
			second := []byte(`{"model":"claude-opus-5-5-high","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"calculate"},{"role":"assistant","content":[]},{"role":"user","content":"plus 17"}]}`)
			second, _ = sjson.SetRawBytes(second, "messages.1.content", content)
			call(second)
			upstream := <-requests
			parts := gjson.GetBytes(upstream, "request.contents.1.parts").Array()
			if len(parts) < 2 || !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != "checked calculation" || parts[0].Get("thoughtSignature").String() != sig {
				t.Fatalf("second turn lost Q reasoning or changed signature: %s", upstream)
			}
		})
	}
}
