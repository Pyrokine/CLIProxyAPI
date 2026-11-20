package auth

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalRequestRuleConfigIndexAndCompatibilitySources(t *testing.T) {
	configured := []config.RequestScopedErrorRule{
		{
			Status: 503, Match: []string{"configured"}, Action: RequestScopedActionStop,
		},
	}
	oauth := []config.RequestScopedErrorRule{
		{
			Status: 503, Match: []string{"oauth"}, Action: RequestScopedActionContinue,
		},
	}
	cfg := &config.Config{
		GeminiKey:       []config.GeminiKey{{APIKey: "g", RequestScopedErrors: configured}},
		InteractionsKey: []config.GeminiKey{{APIKey: "i", RequestScopedErrors: configured}},
		ClaudeKey:       []config.ClaudeKey{{APIKey: "c", RequestScopedErrors: configured}},
		CodexKey:        []config.CodexKey{{APIKey: "x", RequestScopedErrors: configured}},
		XAIKey:          []config.CodexKey{{APIKey: "a", RequestScopedErrors: configured}},
		MetaKey:         []config.MetaKey{{APIKey: "m", RequestScopedErrors: configured}},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "custom", RequestScopedErrors: configured,
			},
		},
		OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{"claude": oauth},
	}
	for _, provider := range []string{
		"gemini", "interactions", "gemini-interactions", "claude", "codex", "xai", "meta",
	} {
		t.Run(
			provider+"/config-index-only", func(t *testing.T) {
				auth := &Auth{Provider: provider, Attributes: map[string]string{AttributeConfigIndex: "0"}}
				if rules := extractRequestScopedErrorRules(
					auth, cfg,
				); len(rules) != 1 || rules[0].Match[0] != "configured" {
					t.Fatalf("index-only rules=%+v", rules)
				}
			},
		)
	}
	for _, provider := range []string{"openai-compatible-custom", "openai-compatibility:custom"} {
		for _, indexed := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/indexed=%t", provider, indexed), func(t *testing.T) {
					auth := &Auth{Provider: provider, Attributes: map[string]string{}}
					if indexed {
						auth.Attributes[AttributeConfigIndex] = "0"
					}
					if rules := extractRequestScopedErrorRules(
						auth, cfg,
					); len(rules) != 1 || rules[0].Match[0] != "configured" {
						t.Fatalf("compatibility provider rules=%+v", rules)
					}
				},
			)
		}
	}
	for _, sample := range []struct {
		name string
		auth *Auth
		want string
	}{
		{
			"compat name", &Auth{
				Provider:   "plugin-provider",
				Attributes: map[string]string{"compat_name": "custom", AttributeConfigIndex: "0"},
			}, "configured",
		},
		{
			"attribute OAuth", &Auth{
				Provider: "claude", Attributes: map[string]string{
					AttributeAuthKind: AuthKindOAuth, AttributeAPIKey: "c", AttributeConfigIndex: "0",
				},
			}, "oauth",
		},
		{
			"metadata OAuth", &Auth{
				Provider: "claude", Attributes: map[string]string{AttributeAPIKey: "c", AttributeConfigIndex: "0"},
				Metadata: map[string]any{AttributeAuthKind: AuthKindOAuth},
			}, "oauth",
		},
		{"invalid index", &Auth{Provider: "claude", Attributes: map[string]string{AttributeConfigIndex: "-1"}}, ""},
		{
			"changed key", &Auth{
				Provider: "claude", Attributes: map[string]string{
					AttributeAPIKey:      "different",
					AttributeConfigIndex: "0",
				},
			}, "",
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				rules := extractRequestScopedErrorRules(sample.auth, cfg)
				if sample.want == "" {
					if len(rules) != 0 {
						t.Fatalf("unbound configuration supplied rules=%+v", rules)
					}
					return
				}
				if len(rules) != 1 || rules[0].Match[0] != sample.want {
					t.Fatalf("rules=%+v, want %s", rules, sample.want)
				}
			},
		)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestOriginalRequestRuleForcedCooldownRetainsRetryAfter(t *testing.T) {
	for _, scope := range []core.StateEffectScope{core.StateEffectScopeExactModel, core.StateEffectScopeCredential} {
		for _, setting := range []int{-1, 0, 7} {
			for _, withHint := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/setting=%d/hint=%t", scope, setting, withHint), func(t *testing.T) {
						previous := transientErrorCooldownSeconds.Load()
						SetTransientErrorCooldownSeconds(setting)
						t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
						const id, provider, model = "forced-original-auth", "forced-original-provider", "forced-" +
							"original-model"
						want := time.Minute
						if setting > 0 {
							want = time.Duration(setting) * time.Second
						}
						failure := &requestScopedTestError{
							status: http.StatusServiceUnavailable, message: "forced-original",
						}
						if withHint {
							want = 2 * time.Minute
							failure.retryAfter = &want
						}
						key := model
						if scope == core.StateEffectScopeCredential {
							key = id
						}
						upstream := &requestScopedDispositionError{
							requestScopedTestError: failure, disposition: core.ExecutionDisposition{
								DispatchPhase:  core.DispatchPhaseResponseStarted,
								Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
								RetryDirective: core.RetryDirectiveNextAuth, EffectScope: scope, EffectKey: key,
							},
						}
						executor := &requestScopedTestExecutor{
							provider: provider,
							execute:  func(*Auth) (core.Response, error) { return core.Response{}, upstream },
						}
						manager := NewManager(nil, &FillFirstSelector{}, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						manager.RegisterExecutor(executor)
						registerRequestScopedTestAuth(
							t, manager, &Auth{
								ID: id, Provider: provider, Status: StatusActive,
								Metadata: map[string]any{
									"request_scoped_errors": []config.RequestScopedErrorRule{
										{
											Status: 503, Match: []string{"forced-original"},
											Action: RequestScopedActionStopAndCooldown,
										},
									},
								},
							}, model,
						)
						before := time.Now()
						_, err := manager.Execute(
							t.Context(), []string{provider}, core.Request{Model: model}, core.Options{},
						)
						if err != upstream || executor.calls.Load() != 1 {
							t.Fatalf("error=%v calls=%d", err, executor.calls.Load())
						}
						current, _ := manager.GetByID(id)
						next := current.NextRetryAfter
						if scope == core.StateEffectScopeExactModel {
							state := current.ModelStates[model]
							if state == nil || state.EffectScope != scope || !state.Unavailable {
								t.Fatalf("forced model state=%+v", state)
							}
							next = state.NextRetryAfter
						} else if current.StateEffectScope != scope || !current.Unavailable {
							t.Fatalf("forced credential state=%+v", current)
						}
						if next.Before(before.Add(want)) || next.After(time.Now().Add(want)) ||
							current.Failed != 1 {
							t.Fatalf("retry=%v, want delay=%s, failures=%d", next, want, current.Failed)
						}
					},
				)
			}
		}
	}
}
