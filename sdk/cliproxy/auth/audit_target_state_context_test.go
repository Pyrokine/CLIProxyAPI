package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type auditAliasTargetExecutor struct {
	schedulerTestExecutor
	calls atomic.Int32
}

func (*auditAliasTargetExecutor) Identifier() string { return "claude" }
func (e *auditAliasTargetExecutor) Execute(
	context.Context,
	*Auth,
	executor.Request,
	executor.Options,
) (executor.Response, error) {
	e.calls.Add(1)
	return executor.Response{Payload: []byte(`{}`)}, nil
}
func (e *auditAliasTargetExecutor) CountTokens(
	ctx context.Context,
	a *Auth,
	r executor.Request,
	o executor.Options,
) (executor.Response, error) {
	return e.Execute(ctx, a, r, o)
}
func (e *auditAliasTargetExecutor) ExecuteStream(
	context.Context,
	*Auth,
	executor.Request,
	executor.Options,
) (*executor.StreamResult, error) {
	e.calls.Add(1)
	ch := make(chan executor.StreamChunk, 1)
	ch <- executor.StreamChunk{Payload: []byte(`{}`)}
	close(ch)
	return &executor.StreamResult{Chunks: ch}, nil
}

func TestAuditAliasTargetCooldownBlocksAllManagerEntries(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted", "fill-first"} {
		for _, mode := range []string{"execute", "count", "stream"} {
			for _, blocked := range []bool{false, true} {
				t.Run(
					strategy+"/"+mode+map[bool]string{false: "/available", true: "/cooling"}[blocked],
					func(t *testing.T) {
						var selector Selector = &RoundRobinSelector{}
						if strategy == "weighted" {
							selector = &WeightedRoundRobinSelector{}
						}
						if strategy == "fill-first" {
							selector = &FillFirstSelector{}
						}
						manager := NewManager(nil, selector, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						manager.SetOAuthModelAlias(
							map[string][]config.OAuthModelAlias{
								"claude": {
									{
										Name: "target-model", Alias: "public-model",
									},
								},
							},
						)
						capture := &auditAliasTargetExecutor{}
						manager.RegisterExecutor(capture)
						credential, err := manager.Register(
							t.Context(), &Auth{
								ID: t.Name(), Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"},
								Metadata: map[string]any{"access_token": "synthetic"},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						reg := registry.GetGlobalRegistry()
						reg.RegisterClient(
							credential.ID, "claude", []*registry.ModelInfo{{ID: "public-model"}, {ID: "target-model"}},
						)
						t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
						manager.RefreshSchedulerEntry(credential.ID)
						if target := manager.ResolveExecutionModel(
							credential, "public-model",
						); target != "target-model" {
							t.Fatalf("alias target=%s", target)
						}
						if blocked {
							manager.MarkResult(
								t.Context(), Result{
									AuthID: credential.ID, AuthGeneration: credential.Generation, Provider: "claude",
									Model: "target-model",
									Error: &Error{
										HTTPStatus: http.StatusTooManyRequests,
										Message:    "synthetic quota",
									},
									RetryAfter: new(time.Hour),
								},
							)
						}
						request := executor.Request{Model: "public-model"}
						switch mode {
						case "execute":
							_, err = manager.Execute(t.Context(), []string{"claude"}, request, executor.Options{})
						case "count":
							_, err = manager.ExecuteCount(t.Context(), []string{"claude"}, request, executor.Options{})
						case "stream":
							var result *executor.StreamResult
							result, err = manager.ExecuteStream(
								t.Context(), []string{"claude"}, request, executor.Options{},
							)
							if err == nil {
								for range result.Chunks {
								}
							}
						}
						if blocked {
							if err == nil || capture.calls.Load() != 0 {
								t.Fatalf("cooling target executed: calls=%d error=%v", capture.calls.Load(), err)
							}
						} else if err != nil || capture.calls.Load() != 1 {
							t.Fatalf("available target calls=%d error=%v", capture.calls.Load(), err)
						}
					},
				)
			}
		}
	}
}

type auditUsageContext struct {
	alias, effort, tier string
	generate            bool
}

func auditContext(ctx context.Context) auditUsageContext {
	return auditUsageContext{
		usage.RequestedModelAliasFromContext(ctx), usage.ReasoningEffortFromContext(ctx),
		usage.ServiceTierFromContext(ctx), usage.GenerateFromContext(ctx),
	}
}

type auditPreparationHook struct{ seen chan auditUsageContext }

func (*auditPreparationHook) OnAuthRegistered(context.Context, *Auth)  {}
func (*auditPreparationHook) OnAuthUpdated(context.Context, *Auth)     {}
func (h *auditPreparationHook) OnResult(ctx context.Context, _ Result) { h.seen <- auditContext(ctx) }

type auditHomePrepareFailure struct {
	homeCapabilityCaptureExecutor
	seen auditUsageContext
}

func (*auditHomePrepareFailure) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *auditHomePrepareFailure) PrepareRequestAuth(ctx context.Context, _ *Auth) (*Auth, error) {
	e.seen = auditContext(ctx)
	return nil, &Error{Code: "synthetic_prepare_failure", Message: "synthetic", HTTPStatus: http.StatusBadGateway}
}

func TestAuditHomePreparationFailurePreservesAttemptUsageContext(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				hook := &auditPreparationHook{seen: make(chan auditUsageContext, 4)}
				manager := NewManager(nil, nil, hook)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				capture := &auditHomePrepareFailure{}
				manager.RegisterExecutor(capture)
				raw, err := json.Marshal(
					homeAuthDispatchResponse{
						Model: "upstream-model", Auth: Auth{
							ID: t.Name(), Provider: "test", Metadata: map[string]any{"access_token": "synthetic"},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				manager.PublishHomeDispatch(
					&testHomeDispatcher{heartbeat: true, payload: raw}, executionregistry.New(), 1,
				)
				opts := executor.Options{
					Metadata: map[string]any{
						executor.RequestedModelMetadataKey:  "visible-alias",
						executor.ReasoningEffortMetadataKey: "high", executor.ServiceTierMetadataKey: "priority",
						executor.GenerateMetadataKey: false,
					},
				}
				req := executor.Request{Model: "route-model"}
				switch mode {
				case "execute":
					_, err = manager.Execute(t.Context(), []string{"test"}, req, opts)
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{"test"}, req, opts)
				case "stream":
					_, err = manager.ExecuteStream(t.Context(), []string{"test"}, req, opts)
				}
				if err == nil {
					t.Fatal("synthetic preparation failure was accepted")
				}
				want := auditUsageContext{"visible-alias", "high", "priority", false}
				if capture.seen != want {
					t.Fatalf("prepare context=%+v", capture.seen)
				}
				select {
				case actual := <-hook.seen:
					if actual != want {
						t.Fatalf("result context=%+v want=%+v", actual, want)
					}
				default:
					t.Fatal("preparation failure result not observed")
				}
			},
		)
	}
}
