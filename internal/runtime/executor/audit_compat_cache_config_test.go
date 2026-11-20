package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditOpenAICompatCacheConfigurationRetainsFinalControl(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, filter := range []bool{false, true} {
			received := make(chan []byte, 1)
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						received <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(
								w,
								"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":"+
									"\"stop\"}]}\n\ndata: "+
									"[DONE]\n\n",
							)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(
								w,
								`{"choices":[{"index":0,"message":{"role":"assistant","content":`+
									`"synthetic"},"finish_reason":`+
									`"stop"}]}`,
							)
						}
					},
				),
			)
			cfg := &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{
					{
						Name: "audit-compatible", SupportPromptCacheKey: true,
					},
				},
			}
			models := []config.PayloadModelRule{{Name: "initial-model", Protocol: "openai"}}
			if filter {
				cfg.Payload.Filter = []config.PayloadFilterRule{
					{Models: models, Params: []string{"prompt_cache_key"}},
				}
			} else {
				cfg.Payload.Override = []config.PayloadRule{
					{
						Models: models, Params: map[string]any{"prompt_cache_key": "operator-cache"},
					},
				}
			}
			credential := runtimeCodexAuth(server.URL)
			credential.Provider = "audit-compatible"
			credential.Attributes["compat_name"] = "audit-compatible"
			req := core.Request{
				Model: "initial-model",
				Payload: []byte(
					`{"model":"initial-model","prompt_cache_key":"client-cache","messages":` +
						`[{"role":"user","content":` +
						`"synthetic"}]}`,
				),
			}
			opts := core.Options{SourceFormat: translator.FormatOpenAI}
			execute := NewOpenAICompatExecutor("audit-compatible", cfg)
			var err error
			if stream {
				var result *core.StreamResult
				result, err = execute.ExecuteStream(t.Context(), credential, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = execute.Execute(t.Context(), credential, req, opts)
			}
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			body := <-received
			key := gjson.GetBytes(body, "prompt_cache_key")
			if filter && key.Exists() || !filter && key.String() != "operator-cache" {
				t.Fatalf("stream=%t filter=%t body=%s", stream, filter, body)
			}
		}
	}
}
