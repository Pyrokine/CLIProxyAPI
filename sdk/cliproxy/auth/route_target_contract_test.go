package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type routeContractSelector struct {
	t               *testing.T
	provider, model string
	calls           int
}

func (s *routeContractSelector) Pick(_ context.Context, provider, model string, _ core.Options, auths []*Auth) (
	*Auth,
	error,
) {
	s.t.Helper()
	s.calls++
	if provider != s.provider || model != s.model || len(auths) != 1 {
		s.t.Fatalf(
			"selector provider/model/candidates=%q/%q/%d, want %q/%q/1", provider, model, len(auths), s.provider,
			s.model,
		)
	}
	return auths[0].Clone(), nil
}

func TestRouteAliasSelectionUsesRegisteredTargetAndOriginalSelectorModel(t *testing.T) {
	for _, strategy := range []string{"custom", "round-robin", "fill-first", "weighted", "quota-aware"} {
		for _, mixed := range []bool{false, true} {
			for _, registeredModel := range []string{"target", "route", "both", "unsupported"} {
				for _, cooling := range []bool{false, true} {
					name := fmt.Sprintf(
						"%s/mixed=%t/registered=%s/cooling=%t", strategy, mixed, registeredModel, cooling,
					)
					t.Run(
						name, func(t *testing.T) {
							const route, target, requested = "public-model", "target-model", "public-model(high)"
							provider := "claude"
							if mixed {
								provider = "mixed"
							}
							capture := &routeContractSelector{t: t, provider: provider, model: requested}
							var selector Selector = capture
							switch strategy {
							case "round-robin":
								selector = &RoundRobinSelector{}
							case "fill-first":
								selector = &FillFirstSelector{}
							case "weighted":
								selector = &WeightedRoundRobinSelector{}
							case "quota-aware":
								selector = &QuotaAwareSelector{}
							}
							manager := NewManager(nil, selector, nil)
							manager.SetHomeClientProvider(func() HomeClient { return nil })
							manager.SetOAuthModelAlias(
								map[string][]config.OAuthModelAlias{
									"claude": {
										{
											Name: target, Alias: route,
										},
									},
								},
							)
							manager.RegisterExecutor(&auditAliasTargetExecutor{})
							manager.executors["unused"] = schedulerTestExecutor{}
							credential := &Auth{
								ID: t.Name(), Provider: "claude", Status: StatusActive,
								Attributes: map[string]string{"auth_kind": "oauth"},
								Metadata:   map[string]any{"access_token": "synthetic"},
							}
							if cooling {
								deadline := time.Now().Add(time.Hour)
								credential.ModelStates = map[string]*ModelState{
									target: {
										Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
										Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
									},
								}
							}
							current, err := manager.Register(t.Context(), credential)
							if err != nil {
								t.Fatal(err)
							}
							if got := manager.ResolveExecutionModel(current, requested); got != target+"(high)" {
								t.Fatalf("resolved target=%q", got)
							}
							var models []*registry.ModelInfo
							if registeredModel == "target" || registeredModel == "both" {
								models = append(models, &registry.ModelInfo{ID: target})
							}
							if registeredModel == "route" || registeredModel == "both" {
								models = append(models, &registry.ModelInfo{ID: route})
							}
							registerProjectionTestClient(t, current.ID, current.Provider, models...)
							manager.RefreshSchedulerEntry(current.ID)
							var selected *Auth
							if mixed {
								var selectedProvider string
								selected, _, selectedProvider, err = manager.pickNextMixed(
									t.Context(), []string{"claude", "unused"}, requested, core.Options{}, nil,
								)
								if selected != nil && selectedProvider != "claude" {
									t.Fatalf("selected provider=%q", selectedProvider)
								}
							} else {
								selected, _, err = manager.PickNext(
									t.Context(), "claude", requested, core.Options{}, nil,
								)
							}
							wantAvailable := registeredModel != "unsupported" && !cooling
							if wantAvailable && (err != nil || selected == nil || selected.ID != current.ID) ||
								!wantAvailable && (err == nil || selected != nil) {
								t.Fatalf("available=%t selected=%#v error=%v", wantAvailable, selected, err)
							}
							if strategy == "custom" {
								wantCalls := 0
								if wantAvailable {
									wantCalls = 1
								}
								if capture.calls != wantCalls {
									t.Fatalf(
										"custom selector calls=%d, want %d after target eligibility", capture.calls,
										wantCalls,
									)
								}
							}
						},
					)
				}
			}
		}
	}
}

func TestRouteAliasUpdatesReevaluateExistingModelShard(t *testing.T) {
	const route, first, second = "public-model", "first-target", "second-target"
	manager := NewManager(nil, &QuotaAwareSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: first, Alias: route}}})
	manager.RegisterExecutor(&auditAliasTargetExecutor{})
	credential, err := manager.Register(
		t.Context(), &Auth{
			ID: t.Name(), Provider: "claude", Status: StatusActive,
			Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"access_token": "synthetic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	reg := registerProjectionTestClient(
		t, credential.ID, credential.Provider,
		&registry.ModelInfo{ID: route}, &registry.ModelInfo{ID: first}, &registry.ModelInfo{ID: second},
	)
	manager.RefreshSchedulerEntry(credential.ID)
	pick := func(available bool) {
		t.Helper()
		selected, _, err := manager.PickNext(t.Context(), "claude", route, core.Options{}, nil)
		if available && (err != nil || selected == nil || selected.ID != credential.ID) ||
			!available && (err == nil || selected != nil) {
			t.Fatalf("available=%t selected=%#v error=%v", available, selected, err)
		}
		if reg.IsModelSuspendedForClient(credential.ID, route) == available ||
			reg.IsModelQuotaExceededForClient(credential.ID, route) == available {
			t.Fatalf("registry and scheduler disagree for available=%t", available)
		}
	}
	pick(true)
	failure := executionResult(
		credential, "claude", first, errorWithState(
			&Error{HTTPStatus: 429}, errorState{EffectScope: core.StateEffectScopeExactModel, EffectKey: first},
		), core.DispatchPhaseResponseStarted,
	)
	failure.RetryAfter = new(time.Hour)
	manager.MarkResult(t.Context(), failure)
	pick(false)
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: second, Alias: route}}})
	pick(true)
	current, _ := manager.GetByID(credential.ID)
	if state := current.ModelStates[first]; state == nil || !state.Quota.Exceeded {
		t.Fatal("changing the route cleared an independent target cooldown")
	}
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: first, Alias: route}}})
	pick(false)
	if _, _, err := manager.ResetCooldown(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	pick(true)
	manager.MarkResult(t.Context(), failure)
	pick(true)
}

func TestRouteAliasContextUsesTargetCapability(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, tc := range []struct {
			name                    string
			routeLimit, targetLimit int
			catalog                 string
			available               bool
		}{
			{"small target", 1_000_000, 128_000, "", false},
			{"large target", 128_000, 1_000_000, "", true},
			{"target only", -1, 1_000_000, "", true},
			{"route names target", 1_000_000, -1, "target-model", true},
			{"unknown target", 1_000_000, -1, "", false},
		} {
			t.Run(
				fmt.Sprintf("%s/mixed=%t", tc.name, mixed), func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
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
					manager.RegisterExecutor(&auditAliasTargetExecutor{})
					credential, err := manager.Register(
						t.Context(), &Auth{
							ID: t.Name(), Provider: "claude", Status: StatusActive,
							Attributes: map[string]string{"auth_kind": "oauth"},
							Metadata:   map[string]any{"access_token": "synthetic"},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					var models []*registry.ModelInfo
					if tc.routeLimit >= 0 {
						models = append(
							models, &registry.ModelInfo{
								ID: "public-model", CatalogName: tc.catalog, ContextLength: tc.routeLimit,
							},
						)
					}
					if tc.targetLimit >= 0 {
						models = append(models, &registry.ModelInfo{ID: "target-model", ContextLength: tc.targetLimit})
					}
					registerProjectionTestClient(t, credential.ID, credential.Provider, models...)
					manager.RefreshSchedulerEntry(credential.ID)
					var selected *Auth
					if mixed {
						selected, _, _, err = manager.pickNextMixed(
							t.Context(), []string{"claude"}, "public-model[1m]", core.Options{}, nil,
						)
					} else {
						selected, _, err = manager.PickNext(
							t.Context(), "claude", "public-model[1m]", core.Options{}, nil,
						)
					}
					if tc.available && (err != nil || selected == nil || selected.ID != credential.ID) {
						t.Fatalf("capable target rejected: selected=%#v error=%v", selected, err)
					}
					if !tc.available {
						requireContext1MModelSupportError(t, err)
						if selected != nil {
							t.Fatal("incapable target selected using route capability")
						}
					}
				},
			)
		}
	}
}

func TestRouteAliasTargetReachesEveryManagerExecutionEntry(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
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
				capture := &homeCapabilityCaptureExecutor{}
				manager.executors["claude"] = capture
				credential, err := manager.Register(
					t.Context(), &Auth{
						ID: t.Name(), Provider: "claude", Status: StatusActive,
						Attributes: map[string]string{"auth_kind": "oauth"},
						Metadata:   map[string]any{"access_token": "synthetic"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				registerProjectionTestClient(
					t, credential.ID, credential.Provider, &registry.ModelInfo{ID: "target-model"},
				)
				manager.RefreshSchedulerEntry(credential.ID)
				request := core.Request{Model: "public-model(high)"}
				switch mode {
				case "execute":
					_, err = manager.Execute(t.Context(), []string{"claude"}, request, core.Options{})
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{"claude"}, request, core.Options{})
				case "stream":
					var result *core.StreamResult
					result, err = manager.ExecuteStream(t.Context(), []string{"claude"}, request, core.Options{})
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				if err != nil || len(capture.requests) != 1 || capture.requests[0].Model != "target-model(high)" ||
					request.Model != "public-model(high)" {
					t.Fatalf("final execution error=%v requests=%#v original=%q", err, capture.requests, request.Model)
				}
			},
		)
	}
}
