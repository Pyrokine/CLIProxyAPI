package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestOriginalSessionAffinityAliasCooldownPreservesSelection(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "fill-first"} {
		for _, mode := range []string{"no-session", "explicit-session", "lcp"} {
			for _, entry := range []string{"select", "execute", "stream"} {
				t.Run(
					strategy+"/"+mode+"/"+entry, func(t *testing.T) {
						const route, target = "affinity-alias", "affinity-healthy-target"
						var fallback Selector = &RoundRobinSelector{}
						switch strategy {
						case "weighted-round-robin":
							fallback = &WeightedRoundRobinSelector{}
						case "fill-first":
							fallback = &FillFirstSelector{}
						}
						selector := NewSessionAffinitySelector(fallback)
						t.Cleanup(selector.Stop)
						manager := NewManager(nil, selector, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						manager.SetRetryConfig(0, 0, 0)
						high, low := "z-high-"+t.Name(), "a-low-"+t.Name()
						cool := func(id, model string) {
							delay := time.Hour
							markOriginalCooldownResult(
								t, manager, Result{
									AuthID: id, Provider: "codex", Model: model, RetryAfter: &delay,
									Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
									Disposition: core.ExecutionDisposition{
										DispatchPhase:  core.DispatchPhaseResponseStarted,
										Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
										RetryDirective: core.RetryDirectiveNoRetry,
										EffectScope:    core.StateEffectScopeExactModel, EffectKey: model,
									},
								},
							)
						}
						for _, auth := range []*Auth{
							{
								ID: high, Provider: "codex", Status: StatusActive,
								Attributes: map[string]string{"priority": "4", AttributeWeight: "1"},
							},
							{
								ID: low, Provider: "codex", Status: StatusActive,
								Attributes: map[string]string{"priority": "3", AttributeWeight: "1"},
							},
						} {
							registerProjectionTestClient(
								t, auth.ID, auth.Provider, &registry.ModelInfo{ID: route},
								&registry.ModelInfo{ID: target},
							)
							if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
								t.Fatal(err)
							}
							cool(auth.ID, route)
						}
						manager.SetOAuthModelAlias(
							map[string][]config.OAuthModelAlias{
								"codex": {
									{
										Name: target, Alias: route, Fork: true,
									},
								},
							},
						)
						var attempts []string
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "codex", execute: func(
									_ context.Context,
									auth *Auth,
									req core.Request,
									opts core.Options,
								) error {
									attempts = append(attempts, auth.ID)
									if req.Model != target {
										t.Errorf("upstream model=%s, want %s", req.Model, target)
									}
									if mode == "lcp" && opts.Metadata[core.LCPAffinitySessionIDMetadataKey] == nil {
										t.Error("LCP selection did not publish a session ID to the executor")
									}
									return nil
								},
							},
						)
						options := func(session string) core.Options {
							opts := core.Options{Metadata: map[string]any{}}
							switch mode {
							case "explicit-session":
								opts.Headers = http.Header{"X-Session-Id": {session}}
							case "lcp":
								opts.SourceFormat = translator.FormatOpenAI
								opts.OriginalRequest = []byte(fmt.Sprintf(
									`{"messages":[{"role":"system","content":%q},{"role":"user","content":"hello"}]}`,
									session,
								))
								opts.Metadata[core.CallerScopeMetadataKey] = "alias-caller"
							}
							return opts
						}
						invoke := func(opts core.Options) (string, error) {
							if entry == "select" {
								auth, err := manager.SelectAuth(t.Context(), "codex", route, opts)
								if err != nil {
									return "", err
								}
								return auth.ID, nil
							}
							before := len(attempts)
							var payload string
							if entry == "stream" {
								opts.Stream = true
								result, err := manager.ExecuteStream(
									t.Context(), []string{"codex"}, core.Request{Model: route}, opts,
								)
								if err != nil {
									return "", err
								}
								var body strings.Builder
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										return "", chunk.Err
									}
									body.Write(chunk.Payload)
								}
								payload = body.String()
							} else {
								response, err := manager.Execute(
									t.Context(), []string{"codex"}, core.Request{Model: route}, opts,
								)
								if err != nil {
									return "", err
								}
								payload = string(response.Payload)
							}
							if payload != "ok" || len(attempts) != before+1 {
								t.Fatalf("payload=%q attempts=%v, want one successful execution", payload, attempts)
							}
							return attempts[before], nil
						}
						assertPick := func(label, session, want string) {
							t.Helper()
							opts := options(session)
							got, err := invoke(opts)
							if err != nil || got != want {
								t.Fatalf("%s: auth=%q error=%v, want %s; attempts=%v", label, got, err, want, attempts)
							}
							if mode == "lcp" && entry == "select" &&
								opts.Metadata[core.LCPAffinitySessionIDMetadataKey] == nil {
								t.Fatal("LCP selection did not publish a session ID")
							}
						}
						assertPick("old alias cooldown with healthy target", "stable", high)
						cool(high, target)
						assertPick("target cooldown selects another credential", "stable", low)
						markOriginalCooldownResult(
							t, manager, Result{AuthID: high, Provider: "codex", Model: target, Success: true},
						)
						if current, ok := manager.GetByID(high); !ok {
							t.Fatal("recovered credential disappeared")
						} else if blocked, _, _ := isAuthBlockedForModel(current, route, time.Now()); !blocked {
							t.Fatal("target recovery cleared the independent old alias cooldown")
						}
						wantAfterRecovery := low
						if mode == "no-session" {
							wantAfterRecovery = high
						}
						assertPick("recovery preserves the current session binding", "stable", wantAfterRecovery)
						assertPick("new session selects the highest priority", "fresh", high)
						cool(high, target)
						cool(low, target)
						before := len(attempts)
						if _, err := invoke(options("stable")); statusCodeFromError(err) != http.StatusTooManyRequests {
							t.Fatalf("all targets cooling: error=%v, want 429", err)
						}
						if len(attempts) != before {
							t.Fatal("all targets were cooling but an executor was called")
						}
					},
				)
			}
		}
	}
}
