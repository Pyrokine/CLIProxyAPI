package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalResolveModelAliasPoolKeepsSuffixAndExactAlias(t *testing.T) {
	for _, tc := range []struct {
		requested string
		models    []modelAliasEntry
		want      []string
	}{
		{
			"claude-opus-4.66(8192)", []modelAliasEntry{
				config.OpenAICompatibilityModel{Name: "deepseek-v3.1", Alias: "claude-opus-4.66"},
				config.OpenAICompatibilityModel{Name: "glm-5", Alias: "claude-opus-4.66"},
				config.OpenAICompatibilityModel{Name: "kimi-k2.5", Alias: "claude-opus-4.66"},
			}, []string{"deepseek-v3.1(8192)", "glm-5(8192)", "kimi-k2.5(8192)"},
		},
		{
			"public(low)", []modelAliasEntry{
				config.OpenAICompatibilityModel{Name: "base-model", Alias: "public"},
				config.OpenAICompatibilityModel{Name: "low-model", Alias: "public(low)", ForceMapping: true},
			}, []string{"low-model(low)"},
		},
	} {
		t.Run(
			tc.requested, func(t *testing.T) {
				pool := resolveModelAliasPoolFromConfigModels(tc.requested, tc.models)
				var got []string
				for _, target := range pool {
					got = append(got, target.UpstreamModel)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("targets=%v, want %v", got, tc.want)
				}
				if tc.requested == "public(low)" &&
					(!pool[0].ForceMapping || pool[0].OriginalAlias != "public(low)") {
					t.Fatalf("exact alias lost its selected force mapping: %+v", pool)
				}
			},
		)
	}
}

func TestModelAliasPoolUnknownAndEmptyResultsStayTerminal(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count", "empty-stream"} {
		t.Run(
			mode, func(t *testing.T) {
				const alias = "claude-opus-4.66"
				upstreamErr := &Error{HTTPStatus: http.StatusBadGateway, Message: "first model upstream failed"}
				internalErr := errors.New("second model failed before upstream")
				provider := &openAICompatPoolExecutor{
					id:                "pool",
					executeErrors:     map[string]error{"deepseek-v3.1": upstreamErr, "glm-5": internalErr},
					countErrors:       map[string]error{"deepseek-v3.1": upstreamErr, "glm-5": internalErr},
					streamFirstErrors: map[string]error{"deepseek-v3.1": upstreamErr, "glm-5": internalErr},
				}
				if mode == "empty-stream" {
					provider.streamFirstErrors = nil
					provider.streamPayloads = map[string][]core.StreamChunk{"deepseek-v3.1": {}}
				}
				manager := newOpenAICompatPoolTestManager(
					t, alias, []config.OpenAICompatibilityModel{
						{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
					}, provider,
				)
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(
						t.Context(),
						[]string{"pool"},
						core.Request{Model: alias},
						core.Options{},
					)
				case "count":
					_, err = manager.ExecuteCount(
						t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{},
					)
				default:
					var result *core.StreamResult
					result, err = manager.ExecuteStream(
						t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{},
					)
					if err == nil {
						for chunk := range result.Chunks {
							if len(chunk.Payload) > 0 {
								t.Fatalf("unknown outcome returned payload=%q", chunk.Payload)
							}
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				models := provider.ExecuteModels()
				if mode == "count" {
					models = provider.CountModels()
				}
				if strings.Contains(mode, "stream") {
					models = provider.StreamModels()
				}
				if err == nil || len(models) != 1 || models[0] != "deepseek-v3.1" {
					t.Fatalf("unknown outcome error=%v models=%v", err, models)
				}
				if mode != "empty-stream" && !errors.Is(err, upstreamErr) {
					t.Fatalf("original upstream cause changed: %v", err)
				}
				if disposition := executionDispositionForError(
					err, core.DispatchPhaseMaybeSent,
				); disposition.Outcome !=
					core.ExecutionOutcomeUnknown ||
					disposition.RetryDirective !=
						core.RetryDirectiveNoRetry {
					t.Fatalf("unknown outcome gained replay authority: %+v", disposition)
				}
			},
		)
	}
}

func TestOriginalModelAliasPoolBlockedAuthAndTargetKeepDispatchBudget(t *testing.T) {
	const alias = "claude-opus-4.66"
	provider := &openAICompatPoolExecutor{id: "pool"}
	manager := newOpenAICompatPoolTestManager(
		t, alias, []config.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
		}, provider,
	)
	if err := manager.Remove(t.Context(), "pool-auth-"+t.Name()); err != nil {
		t.Fatal(err)
	}
	manager.SetRetryConfig(0, 0, 1)
	for _, id := range []string{"aa-blocked-auth", "bb-good-auth"} {
		credential, err := manager.Register(
			t.Context(), &Auth{
				ID: id, Provider: "pool", Status: StatusActive,
				Attributes: map[string]string{
					"api_key":     map[bool]string{true: "bad-key", false: "good-key"}[id == "aa-blocked-auth"],
					"compat_name": "pool", "provider_key": "pool",
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		registerProjectionTestClient(t, credential.ID, "pool", &registry.ModelInfo{ID: alias})
		manager.RefreshSchedulerEntry(credential.ID)
		for _, model := range []string{"deepseek-v3.1", "glm-5"} {
			if id == "bb-good-auth" && model == "glm-5" {
				continue
			}
			markOriginalCooldownResult(
				t, manager, Result{
					AuthID: id, Provider: "pool", Model: model,
					Error: &Error{
						HTTPStatus: http.StatusBadRequest,
						Message:    "invalid_request_error: The requested model is not supported.",
					},
				},
			)
		}
	}
	response, err := manager.Execute(t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != "glm-5" {
		t.Fatalf("available target response=%q error=%v", response.Payload, err)
	}
	provider.mu.Lock()
	ids := append([]string(nil), provider.executeAuthIDs...)
	provider.mu.Unlock()
	if !reflect.DeepEqual(ids, []string{"bb-good-auth"}) || !reflect.DeepEqual(
		provider.ExecuteModels(), []string{"glm-5"},
	) {
		t.Fatalf(
			"blocked candidates consumed a real attempt: auths=%v targets=%v",
			ids,
			provider.ExecuteModels(),
		)
	}
}

func TestModelAliasPoolBindsActualTargetCapabilities(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, scenario := range []string{"rotation", "context-1m", "post-auth-target", "post-auth-unsupported-1m"} {
			t.Run(
				mode+"/"+scenario, func(t *testing.T) {
					const alias = "public-model"
					models := []config.OpenAICompatibilityModel{
						{
							Name: "small-target", Alias: alias, MaxContextLength: 128000, IsCompat: true,
							Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
						},
						{
							Name: "large-target", Alias: alias, MaxContextLength: 2000000,
							Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
						},
					}
					manager := newOpenAICompatPoolTestManager(t, alias, models, nil)
					provider := &homeCapabilityCaptureExecutor{provider: "pool"}
					manager.RegisterExecutor(provider)
					requested := alias
					if strings.Contains(scenario, "1m") {
						requested += "[1m]"
					}
					opts := core.Options{}
					if strings.HasPrefix(scenario, "post-auth") {
						opts.RequestAfterAuthInterceptor = func(
							_ context.Context,
							in core.RequestAfterAuthInterceptRequest,
						) core.RequestAfterAuthInterceptResponse {
							in.Request.Model = "large-target"
							if scenario == "post-auth-unsupported-1m" {
								in.Request.Model = "small-target"
							}
							return core.RequestAfterAuthInterceptResponse{
								Handled: true, Request: in.Request, Options: in.Options,
							}
						}
					}
					calls := 1
					if scenario == "rotation" {
						calls = 2
					}
					for i := range calls {
						var err error
						switch mode {
						case "execute":
							_, err = manager.Execute(
								t.Context(), []string{"pool"}, core.Request{Model: requested}, opts,
							)
						case "count":
							_, err = manager.ExecuteCount(
								t.Context(), []string{"pool"}, core.Request{Model: requested}, opts,
							)
						default:
							var result *core.StreamResult
							result, err = manager.ExecuteStream(
								t.Context(), []string{"pool"}, core.Request{Model: requested}, opts,
							)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						}
						if scenario == "post-auth-unsupported-1m" {
							if statusCodeFromError(err) != http.StatusBadRequest ||
								len(provider.requests) != 0 {
								t.Fatalf(
									"unsupported final target executed: requests=%v error=%v",
									provider.requests,
									err,
								)
							}
							continue
						}
						if err != nil || len(provider.requests) != i+1 {
							t.Fatalf("requests=%v error=%v", provider.requests, err)
						}
						request := provider.requests[i]
						info, bound := ResolvedModelInfo(request)
						wantTarget, wantLevel, wantContext, wantCompat := "large-target", "high", 2000000, false
						if scenario == "rotation" && i == 0 {
							wantTarget, wantLevel, wantContext, wantCompat = "small-target", "low", 128000, true
						}
						if !strings.HasPrefix(request.Model, wantTarget) || !bound || info.ID != wantTarget ||
							info.ContextLength != wantContext || info.IsCompat != wantCompat || info.Thinking == nil ||
							!reflect.DeepEqual(info.Thinking.Levels, []string{wantLevel}) {
							t.Fatalf(
								"actual target=%s capabilities=%+v, want %s/%s/%d/%t",
								request.Model,
								info,
								wantTarget,
								wantLevel,
								wantContext,
								wantCompat,
							)
						}
						if scenario == "context-1m" && !strings.Contains(request.Model, "[1m]") {
							t.Fatalf("request decoration lost: %s", request.Model)
						}
					}
				},
			)
		}
	}
}

func TestModelAliasPoolStreamAndPostAuthUseSelectedForceFlag(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("post-auth=%t", rewrite), func(t *testing.T) {
				const alias = "public-model"
				provider := &openAICompatPoolExecutor{
					id: "pool", streamPayloads: map[string][]core.StreamChunk{
						"first-upstream":  {{Payload: []byte("data: {\"model\":\"first-upstream\"}\n\n")}},
						"second-upstream": {{Payload: []byte("data: {\"model\":\"second-upstream\"}\n\n")}},
					},
				}
				manager := newOpenAICompatPoolTestManager(
					t, alias, []config.OpenAICompatibilityModel{
						{Name: "first-upstream", Alias: alias, ForceMapping: true},
						{Name: "second-upstream", Alias: alias},
					}, provider,
				)
				opts := core.Options{}
				if rewrite {
					opts.RequestAfterAuthInterceptor = func(
						_ context.Context,
						in core.RequestAfterAuthInterceptRequest,
					) core.RequestAfterAuthInterceptResponse {
						in.Request.Model = "second-upstream"
						return core.RequestAfterAuthInterceptResponse{
							Handled: true, Request: in.Request, Options: in.Options,
						}
					}
				}
				for i := range 2 {
					result, err := manager.ExecuteStream(
						t.Context(), []string{"pool"}, core.Request{Model: alias}, opts,
					)
					if err != nil {
						t.Fatal(err)
					}
					want := "second-upstream"
					if !rewrite && i == 0 {
						want = alias
					}
					if payload := readOpenAICompatStreamPayload(
						t, result,
					); payload != "data: {\"model\":\""+want+"\"}\n\n" {
						t.Fatalf("selected force flag returned %q, want model=%s", payload, want)
					}
				}
			},
		)
	}
}

func TestModelAliasPoolRotationStateFollowsConfigAndCredentialLifetime(t *testing.T) {
	const alias = "claude-opus-4.66"
	provider := &openAICompatPoolExecutor{id: "pool"}
	models := []config.OpenAICompatibilityModel{
		{Name: "deepseek-v3.1", Alias: alias},
		{Name: "glm-5", Alias: alias},
	}
	manager := newOpenAICompatPoolTestManager(t, alias, models, provider)
	credentialID := "pool-auth-" + t.Name()
	credential, _ := manager.GetByID(credentialID)
	if _, err := manager.Execute(
		t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{},
	); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	active := len(manager.modelPoolOffsets[credentialID])
	manager.mu.RUnlock()
	if active != 1 {
		t.Fatalf("active pool offsets=%d, want 1", active)
	}
	manager.SetConfig(
		&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "pool", Models: models}}},
	)
	manager.mu.RLock()
	remaining := len(manager.modelPoolOffsets)
	manager.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("configuration publication retained %d pool owners", remaining)
	}
	if _, err := manager.Execute(
		t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{},
	); err != nil {
		t.Fatal(err)
	}
	if got := provider.ExecuteModels(); !reflect.DeepEqual(got, []string{"deepseek-v3.1", "deepseek-v3.1"}) {
		t.Fatalf("fresh configuration rotation=%v", got)
	}
	if err := manager.Remove(t.Context(), credentialID); err != nil {
		t.Fatal(err)
	}
	manager.rotateExecutionModelAliases(
		credential, alias, resolveModelAliasPoolFromConfigModels(alias, asModelAliasEntries(models)),
	)
	manager.mu.RLock()
	remaining = len(manager.modelPoolOffsets)
	manager.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("removed credential retained %d pool owners", remaining)
	}
}

func TestOriginalModelAliasPoolCountSuccessKeepsTargetSuspension(t *testing.T) {
	const alias = "claude-opus-4.66"
	provider := &openAICompatPoolExecutor{
		id: "pool", countErrors: map[string]error{
			"deepseek-v3.1": &Error{
				HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: The requested model is " +
					"unsupported.",
			},
		},
	}
	manager := newOpenAICompatPoolTestManager(
		t, alias, []config.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
		}, provider,
	)
	for range 3 {
		response, err := manager.ExecuteCount(
			t.Context(),
			[]string{"pool"},
			core.Request{Model: alias},
			core.Options{},
		)
		if err != nil {
			t.Fatal(err)
		}
		if string(response.Payload) != "glm-5" {
			t.Fatalf("count response=%s error=%v", response.Payload, err)
		}
	}
	if got := provider.CountModels(); !reflect.DeepEqual(
		got,
		[]string{"deepseek-v3.1", "glm-5", "glm-5", "glm-5"},
	) {
		t.Fatalf("count routing after a model rejection=%v", got)
	}
	credential, _ := manager.GetByID("pool-auth-" + t.Name())
	state := credential.ModelStates["deepseek-v3.1"]
	if len(credential.ModelStates) != 1 || state == nil || !state.Unavailable ||
		state.EffectKey != "deepseek-v3.1" || state.ResetAuthority != StateResetAuthoritySuccess {
		t.Fatalf("successful counts changed the rejected target state: %+v", credential.ModelStates)
	}
}

func TestModelAliasPoolConcurrentRequestsRotateAcrossTargets(t *testing.T) {
	const alias = "claude-opus-4.66"
	provider := &openAICompatPoolExecutor{id: "pool"}
	manager := newOpenAICompatPoolTestManager(
		t, alias, []config.OpenAICompatibilityModel{
			{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
		}, provider,
	)
	var pending sync.WaitGroup
	for range 8 {
		pending.Go(
			func() {
				if _, err := manager.Execute(
					t.Context(), []string{"pool"}, core.Request{Model: alias}, core.Options{},
				); err != nil {
					t.Errorf("concurrent pool execution: %v", err)
				}
			},
		)
	}
	pending.Wait()
	counts := make(map[string]int)
	for _, target := range provider.ExecuteModels() {
		counts[target]++
	}
	if !reflect.DeepEqual(counts, map[string]int{"deepseek-v3.1": 4, "glm-5": 4}) {
		t.Fatalf("concurrent target rotation=%v", counts)
	}
}

func TestModelAliasPoolChecksFinalTargetBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"execute", "count"} {
		for _, blocked := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/blocked=%t", mode, blocked), func(t *testing.T) {
					const alias = "claude-opus-4.66"
					provider := &openAICompatPoolExecutor{
						id: "pool", executeErrors: map[string]error{
							"glm-5": &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
						},
					}
					manager := newOpenAICompatPoolTestManager(
						t, alias, []config.OpenAICompatibilityModel{
							{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
						}, provider,
					)
					id := "pool-auth-" + t.Name()
					credential, _ := manager.GetByID(id)
					credential.Attributes["disable_cooling"] = "true"
					if _, err := manager.Update(t.Context(), credential); err != nil {
						t.Fatal(err)
					}
					if blocked {
						markOriginalCooldownResult(
							t, manager, Result{
								AuthID: id, Provider: "pool", Model: "glm-5",
								Error: &Error{
									HTTPStatus: http.StatusBadRequest,
									Message:    "invalid_request_error: The requested model is not supported.",
								},
							},
						)
					}
					opts := core.Options{
						RequestAfterAuthInterceptor: func(
							_ context.Context,
							in core.RequestAfterAuthInterceptRequest,
						) core.RequestAfterAuthInterceptResponse {
							in.Request.Model = "glm-5"
							return core.RequestAfterAuthInterceptResponse{
								Handled: true, Request: in.Request, Options: in.Options,
							}
						},
					}
					provider.countErrors = provider.executeErrors
					var err error
					var got []string
					if mode == "count" {
						_, err = manager.ExecuteCount(
							t.Context(),
							[]string{"pool"},
							core.Request{Model: alias},
							opts,
						)
						got = provider.CountModels()
					} else {
						_, err = manager.Execute(t.Context(), []string{"pool"}, core.Request{Model: alias}, opts)
						got = provider.ExecuteModels()
					}
					wantCalls := 1
					if blocked {
						wantCalls = 0
					}
					if err == nil || len(got) != wantCalls {
						t.Fatalf(
							"final target blocked=%t calls=%v error=%v, want %d real attempts",
							blocked,
							got,
							err,
							wantCalls,
						)
					}
				},
			)
		}
	}
}

func TestModelAliasPoolWaitUsesEarliestEligibleTarget(t *testing.T) {
	for _, tag := range []string{"", "[1m]"} {
		t.Run(
			"tag="+tag, func(t *testing.T) {
				const alias = "public-model"
				manager := newOpenAICompatPoolTestManager(
					t, alias, []config.OpenAICompatibilityModel{
						{Name: "deepseek-v3.1", Alias: alias, MaxContextLength: 128000},
						{Name: "glm-5", Alias: alias, MaxContextLength: 2000000},
					}, nil,
				)
				id := "pool-auth-" + t.Name()
				for _, tc := range []struct {
					model string
					delay time.Duration
				}{
					{"deepseek-v3.1", 8 * 24 * time.Hour}, {"glm-5", time.Hour},
				} {
					markOriginalCooldownResult(
						t, manager, Result{
							AuthID: id, Provider: "pool", Model: tc.model,
							Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
							RetryAfter: &tc.delay,
						},
					)
				}
				credential, _ := manager.GetByID(id)
				if target := manager.ResolveExecutionModel(credential, alias+tag); target != "glm-5"+tag {
					t.Fatalf("blocked pool target=%s, want earliest eligible glm-5%s", target, tag)
				}
				_, _, err := manager.PickNext(t.Context(), "pool", alias+tag, core.Options{}, nil)
				waitSeconds, parseErr := strconv.Atoi(SafeResponseHeaders(err).Get("Retry-After"))
				if err == nil || parseErr != nil || waitSeconds <= 0 || waitSeconds > 3601 {
					t.Fatalf(
						"blocked pool wait=%d parse=%v error=%v, want earliest eligible deadline", waitSeconds,
						parseErr, err,
					)
				}
			},
		)
	}
}

func TestOriginalCompatPoolPrefixedProviderInputs(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				const providerKey = "openai-compatible-pool"
				const alias = "claude-opus-4.66"
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetConfig(
					&config.Config{
						OpenAICompatibility: []config.OpenAICompatibility{
							{
								Name: "pool", Models: []config.OpenAICompatibilityModel{
									{Name: "deepseek-v3.1", Alias: alias}, {Name: "glm-5", Alias: alias},
								},
							},
						},
					},
				)
				provider := &openAICompatPoolExecutor{id: providerKey}
				manager.RegisterExecutor(provider)
				auth := &Auth{
					ID: "pool-auth-" + t.Name(), Provider: providerKey, Status: StatusActive,
					Attributes: map[string]string{
						"api_key": "test-key", "compat_name": "pool", "provider_key": providerKey,
					},
				}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, providerKey, []*registry.ModelInfo{{ID: alias}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				manager.RefreshSchedulerEntry(auth.ID)
				for _, want := range []string{"deepseek-v3.1", "glm-5"} {
					request := core.Request{Model: alias}
					var response core.Response
					var err error
					switch mode {
					case "execute":
						response, err = manager.Execute(
							t.Context(),
							[]string{providerKey},
							request,
							core.Options{},
						)
					case "count":
						response, err = manager.ExecuteCount(
							t.Context(), []string{providerKey}, request, core.Options{},
						)
					default:
						var stream *core.StreamResult
						stream, err = manager.ExecuteStream(
							t.Context(), []string{providerKey}, request, core.Options{Stream: true},
						)
						if err == nil {
							response.Payload = []byte(readOpenAICompatStreamPayload(t, stream))
						}
					}
					if err != nil || string(response.Payload) != want {
						t.Fatalf(
							"original provider key response=%q error=%v, want %s",
							response.Payload,
							err,
							want,
						)
					}
				}
				got := provider.ExecuteModels()
				if mode == "count" {
					got = provider.CountModels()
				} else if mode == "stream" {
					got = provider.StreamModels()
				}
				if !reflect.DeepEqual(got, []string{"deepseek-v3.1", "glm-5"}) {
					t.Fatalf("original provider key targets=%v", got)
				}
			},
		)
	}
}
