package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type geminiSearchHomeDispatcher struct {
	payload []byte
	calls   int
}

func (*geminiSearchHomeDispatcher) HeartbeatOK() bool { return true }

func (d *geminiSearchHomeDispatcher) RPopAuth(_ context.Context, _ string, _ string, _ http.Header, _ int) ([]byte, error) {
	d.calls++
	return bytes.Clone(d.payload), nil
}

func TestGeminiHomeSelectedNativeSearchReachesExecutor(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex", "gemini-cli", "aistudio"} {
		for _, supported := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/search=%t/stream=%t", provider, supported, stream), func(t *testing.T) {
					const upstream = "gemini-2.5-flash"
					const public = "tenant/gemini-native-home-fixture"
					authID := "gemini-native-home-" + provider
					localSupport := !supported
					catalog := registry.GetGlobalRegistry()
					catalog.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: public, CatalogName: upstream,
						NativeCapabilities: &registry.NativeCapabilities{WebSearch: &localSupport}}})
					t.Cleanup(func() { catalog.UnregisterClient(authID) })
					cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
					manager := cliproxyauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					executor := newGeminiSearchRouteExecutor(cfg, provider)
					manager.RegisterExecutor(executor)
					auth := cliproxyauth.Auth{ID: authID, Provider: provider, Prefix: "tenant", Status: cliproxyauth.StatusActive}
					if provider == "gemini" || provider == "vertex" {
						auth.Attributes = map[string]string{"api_key": "fixture-key", "auth_kind": "apikey"}
					} else {
						auth.Attributes = map[string]string{"auth_kind": "oauth"}
						auth.Metadata = map[string]any{"access_token": "fixture-access", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)}
					}
					wire, err := json.Marshal(map[string]any{
						"model": upstream, "provider": provider, "auth": auth,
						"model_info": map[string]any{"id": upstream, "type": provider, "native_capabilities": map[string]any{"web_search": supported}},
					})
					if err != nil {
						t.Fatal(err)
					}
					dispatcher := &geminiSearchHomeDispatcher{payload: wire}
					manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					payload := []byte(`{"model":"tenant/gemini-native-home-fixture","input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`)
					request := cliproxyexecutor.Request{Model: public, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if stream {
						result, errStream := manager.ExecuteStream(ctx, []string{provider}, request, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := manager.Execute(ctx, []string{provider}, request, opts); err != nil {
						t.Fatal(err)
					}
					if dispatcher.calls != 1 || len(executor.requests) != 1 || len(executor.bodies) != 1 {
						t.Fatalf("Home dispatches=%d, executor requests=%d, bodies=%d", dispatcher.calls, len(executor.requests), len(executor.bodies))
					}
					selected, ok := cliproxyauth.ResolvedModelInfo(executor.requests[0])
					if !ok || selected.ID != upstream || selected.NativeCapabilities == nil || selected.NativeCapabilities.WebSearch == nil || *selected.NativeCapabilities.WebSearch != supported {
						t.Fatalf("Home selected native capability lost: %+v", selected)
					}
					path := "tools.0.googleSearch"
					if provider == "gemini-cli" {
						path = "request." + path
					}
					if search := gjson.GetBytes(executor.bodies[0], path); search.Exists() != supported || supported && search.Get("includedDomains").Raw != `["example.com"]` {
						t.Fatalf("Home native capability not consumed by executor: %s", executor.bodies[0])
					}
				})
			}
		}
	}
}
