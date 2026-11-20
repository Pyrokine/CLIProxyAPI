package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type coolingHomeClient struct {
	home.KVClient
	payload       []byte
	reads, writes int
}

func (*coolingHomeClient) Enabled() bool     { return true }
func (*coolingHomeClient) HeartbeatOK() bool { return true }
func (c *coolingHomeClient) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	return c.payload, nil
}
func (*coolingHomeClient) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("unexpected Home refresh")
}
func (c *coolingHomeClient) KVGet(context.Context, string) ([]byte, bool, error) {
	c.reads++
	return []byte("9999999999999999999"), true, nil
}
func (c *coolingHomeClient) KVSet(context.Context, string, []byte, home.KVSetOptions) (bool, error) {
	c.writes++
	return true, nil
}

func TestAntigravityDisableCoolingOriginalInputsAtManagerEntries(t *testing.T) {
	const model = "claude-sonnet-4-5"
	for _, owner := range []string{"local-global", "local-auth-override", "home-global", "home-auth-override"} {
		for _, entry := range []string{"execute", "count", "stream"} {
			t.Run(
				owner+"/"+entry, func(t *testing.T) {
					cfg := &config.Config{DisableCooling: strings.HasSuffix(owner, "global")}
					cfg.Home.Enabled = strings.HasPrefix(owner, "home")
					credential := antigravityCountTokensAuth(
						"https://cooling.synthetic.invalid", "home-cooling-disabled-auth",
					)
					if strings.HasSuffix(owner, "auth-override") {
						credential.Metadata["disable_cooling"] = true
					}
					client := &coolingHomeClient{}
					manager := cliproxyauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.RegisterExecutor(NewAntigravityExecutor(cfg))
					if cfg.Home.Enabled {
						var err error
						client.payload, err = json.Marshal(
							map[string]any{
								"auth": credential, "model": model, "request_retry": 0,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return client })
						manager.PublishHomeDispatch(client, executionregistry.New(), 1)
					} else {
						manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
						var err error
						credential, err = manager.Register(cliproxyauth.WithSkipPersist(t.Context()), credential)
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(
							credential.ID, "antigravity", []*registry.ModelInfo{{ID: model}},
						)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
					}
					var calls atomic.Int32
					ctx := util.WithRoundTripper(
						t.Context(), antigravityCountTokensRoundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								calls.Add(1)
								return &http.Response{
									StatusCode: http.StatusTooManyRequests, Request: req,
									Header: http.Header{
										"Content-Type": {"application/json"},
										"Retry-After":  {"10"},
									},
									Body: io.NopCloser(
										strings.NewReader(
											`{"error":{"code":429,"message":"Rate limit exceeded","status":"` +
												`RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.` +
												`com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`,
										),
									),
								}, nil
							},
						),
					)
					request := core.Request{
						Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
					}
					options := core.Options{SourceFormat: translator.FormatAntigravity}
					for range 2 {
						var err error
						switch entry {
						case "execute":
							_, err = manager.Execute(ctx, []string{"antigravity"}, request, options)
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{"antigravity"}, request, options)
						case "stream":
							_, err = manager.ExecuteStream(ctx, []string{"antigravity"}, request, options)
						}
						if testExecutorStatusCode(err) != http.StatusTooManyRequests {
							t.Fatalf("expected original 429: %v", err)
						}
					}
					if calls.Load() != 2 || client.reads != 0 || client.writes != 0 {
						t.Fatalf(
							"provider calls=%d Home reads=%d writes=%d",
							calls.Load(),
							client.reads,
							client.writes,
						)
					}
					if !cfg.Home.Enabled {
						current, ok := manager.GetByID(credential.ID)
						if !ok || current.Unavailable || !current.NextRetryAfter.IsZero() {
							t.Fatalf("disabled cooling blocked credential: %+v", current)
						}
					}
				},
			)
		}
	}
}

func TestAntigravityDisableCoolingOriginalInputsSkipCreditsSideEffects(t *testing.T) {
	for _, owner := range []string{"home-global", "auth-override"} {
		t.Run(
			owner, func(t *testing.T) {
				cfg := &config.Config{
					DisableCooling: owner == "home-global",
					QuotaExceeded:  config.QuotaExceeded{AntigravityCredits: true},
				}
				cfg.Home.Enabled = owner == "home-global"
				credential := antigravityCountTokensAuth("https://cooling.synthetic.invalid", t.Name())
				if owner == "auth-override" {
					credential.Metadata["disable_cooling"] = true
				}
				var calls atomic.Int32
				ctx := util.WithRoundTripper(
					t.Context(), antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							return &http.Response{
								StatusCode: http.StatusOK, Request: req, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(`{}`)),
							}, nil
						},
					),
				)
				NewAntigravityExecutor(cfg).maybeRefreshAntigravityCreditsHint(ctx, credential, "token", false)
				if state, ok := antigravityCreditsHintRefreshByID.Load(credential.ID); ok {
					if state, ok := state.(*antigravityCreditsHintRefreshState); ok {
						state.mu.Lock()
						//lint:ignore SA2001 等待意外启动的刷新释放状态锁，再报告测试失败
						state.mu.Unlock()
					}
					t.Fatalf("disabled cooling scheduled credits refresh; HTTP calls=%d", calls.Load())
				}
				if calls.Load() != 0 {
					t.Fatalf("disabled cooling sent credits refresh: %d", calls.Load())
				}
				creditsCtx := cliproxyauth.WithAntigravityCredits(ctx)
				client := &http.Client{
					Transport: antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusTooManyRequests, Request: req, Header: make(http.Header),
								Body: io.NopCloser(
									strings.NewReader(
										`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInf` +
											`o","reason":"INSUFFICIENT_G1_CREDITS_BALANCE"}]}}`,
									),
								),
							}, nil
						},
					),
				}
				_, err := NewAntigravityExecutor(cfg).sendWithRetry(
					creditsCtx, client,
					[]string{"https://cooling.synthetic.invalid"}, 1, "claude-sonnet-4-5", credential,
					func(base string) (*http.Request, error) {
						return http.NewRequestWithContext(creditsCtx, http.MethodPost, base, nil)
					},
				)
				if testExecutorStatusCode(err) != http.StatusTooManyRequests {
					t.Fatalf("credits response lost original 429: %v", err)
				}
				if hint, ok := cliproxyauth.GetAntigravityCreditsHint(credential.ID); ok {
					t.Fatalf("disabled cooling stored a credits disable hint: %+v", hint)
				}
			},
		)
	}
}
