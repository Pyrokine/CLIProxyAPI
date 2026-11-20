package executor

import (
	"bytes"
	"context"
	"fmt"
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

func TestQwenExecutionPreservesDeclaredToolsAndSystemOrder(t *testing.T) {
	for _, fixture := range []struct {
		name, tools string
	}{
		{name: "absent"},
		{name: "empty", tools: `[]`},
		{name: "null", tools: `null`},
		{
			name:  "declared",
			tools: `[{"type":"function","function":{"name":"fixture_lookup","parameters":{"type":"object"}}}]`,
		},
	} {
		for _, hasSystem := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				t.Run(
					fmt.Sprintf(
						"%s/system=%t/stream=%t",
						fixture.name,
						hasSystem,
						streaming,
					),
					func(t *testing.T) {
						messages := `[{"role":"user","content":"fixture user"},{"role":"assistant",` +
							`"content":"fixture ` +
							`assistant"}]`
						if hasSystem {
							messages = `[{"role":"system","content":"first policy"},{"role":"user","content":` +
								`"fixture user"},{"role":"system","content":[{"type":"text","text":"second ` +
								`policy"}]},{"role":"assistant","content":"fixture assistant"}]`
						}
						payload := []byte(`{"model":"qwen3-coder-plus","messages":` + messages)
						if fixture.tools != "" {
							payload = append(payload, []byte(`,"tools":`+fixture.tools)...)
						}
						payload = append(payload, '}')
						original := bytes.Clone(payload)
						calls := 0
						transport := &auditClaudeContractTransport{
							respond: func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Host != "qwen-tools.synthetic.invalid" ||
									request.URL.Path != "/v1/chat/completions" {
									t.Fatalf("unexpected upstream URL: %s", request.URL)
								}
								body, err := io.ReadAll(request.Body)
								if err != nil {
									return nil, err
								}
								if got := gjson.GetBytes(body, "tools"); got.Raw != fixture.tools {
									t.Errorf("wire tools=%s, want %s", got.Raw, fixture.tools)
								}
								if bytes.Contains(body, []byte("do_not_call_me")) {
									t.Error("wire contains an undeclared tool")
								}
								actualMessages := gjson.GetBytes(body, "messages").Array()
								if len(actualMessages) != 3 ||
									actualMessages[0].Get("role").String() != "system" ||
									actualMessages[1].Get("content").String() != "fixture user" ||
									actualMessages[2].Get("content").String() != "fixture assistant" {
									t.Fatalf("wire message order changed: %s", body)
								}
								parts := actualMessages[0].Get("content").Array()
								wantParts := 1
								if hasSystem {
									wantParts = 3
								}
								if len(parts) != wantParts ||
									parts[0].Get("cache_control.type").String() != "ephemeral" {
									t.Fatalf("default system content changed: %s", body)
								}
								if hasSystem &&
									(parts[1].Get("text").String() !=
										"first policy" ||
										parts[2].Get("text").String() !=
											"second policy") {
									t.Errorf("existing system order changed: %s", body)
								}
								if streaming && !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
									t.Error("stream usage was disabled")
								}
								wire := `{"id":"fixture","model":"qwen3-coder-plus","choices":[{"index":0,"mess` +
									`age":{"role":"assistant","content":"fixture answer"},"finish_reason":"` +
									`stop"}]}`
								if streaming {
									wire = "data: {\"id\":\"fixture\",\"model\":\"qwen3-coder-plus\",\"" +
										"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture answer\"}," +
										"\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						}
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						ctx = util.WithRoundTripper(ctx, transport)
						credential := &coreauth.Auth{
							ID: t.Name(), Provider: "qwen", Metadata: map[string]any{
								"access_token": "synthetic-token",
								"resource_url": "https://qwen-tools.synthetic.invalid/v1",
							},
						}
						t.Cleanup(
							func() {
								qwenRateLimiter.Lock()
								defer qwenRateLimiter.Unlock()
								delete(qwenRateLimiter.requests, credential.ID)
							},
						)
						cfg := &config.Config{}
						if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
							t.Fatal("Qwen transport is not memory-isolated")
						}
						err := auditFinalUsageCall(
							ctx, NewQwenExecutor(cfg), credential,
							core.Request{Model: "qwen3-coder-plus", Payload: payload}, core.Options{
								SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI,
							}, streaming,
						)
						if err != nil || calls != 1 {
							t.Fatalf("execution error=%v calls=%d, want one request", err, calls)
						}
						if !bytes.Equal(payload, original) {
							t.Fatal("execution mutated the caller payload")
						}
					},
				)
			}
		}
	}
}
