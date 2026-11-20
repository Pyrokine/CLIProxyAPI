package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalManagerAliasQuotaFailoverWithUnobservedTargetModel(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	SetQuotaCooldownDisabled(false)
	t.Cleanup(func() { SetQuotaCooldownDisabled(previous) })
	for name, newSelector := range map[string]func() Selector{
		"round-robin":          func() Selector { return &RoundRobinSelector{} },
		"weighted-round-robin": func() Selector { return &WeightedRoundRobinSelector{} },
		"fill-first":           func() Selector { return &FillFirstSelector{} },
	} {
		for _, path := range []string{"select", "execute", "stream"} {
			t.Run(
				name+"/"+path, func(t *testing.T) {
					const routeModel, targetModel, otherModel = "quota-route", "quota-target", "quota-other"
					hook := &resultCaptureHook{}
					manager := NewManager(nil, newSelector(), hook)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetRetryConfig(3, 30*time.Second, 0)
					manager.SetOAuthModelAlias(
						map[string][]internalconfig.OAuthModelAlias{
							"codex": {{Name: targetModel, Alias: routeModel, Fork: true}},
						},
					)
					highID, lowID := "alias-quota-high-"+t.Name(), "alias-quota-low-"+t.Name()
					for _, candidate := range []*Auth{
						{
							ID: highID, Provider: "codex", Status: StatusActive,
							Attributes: map[string]string{"priority": "4", AttributeWeight: "1"},
						},
						{
							ID: lowID, Provider: "codex", Status: StatusActive,
							Attributes: map[string]string{"priority": "3", AttributeWeight: "1"},
						},
					} {
						registry.GetGlobalRegistry().RegisterClient(
							candidate.ID, "codex",
							[]*registry.ModelInfo{{ID: routeModel}, {ID: targetModel}, {ID: otherModel}},
						)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
						if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					retryAfter := time.Hour
					manager.MarkResult(
						context.Background(), Result{
							AuthID: highID, Provider: "codex", Model: otherModel,
							Error: &Error{
								HTTPStatus: http.StatusTooManyRequests,
								Message:    "other model rate limit",
							},
							RetryAfter: &retryAfter,
						},
					)
					high, _ := manager.GetByID(highID)
					if !high.Unavailable || high.ModelStates[targetModel] != nil {
						t.Fatal("expected aggregate cooldown with no state yet for the requested target")
					}
					executor := &originalAliasTargetExecutor{id: "codex", exhaustedID: highID}
					manager.RegisterExecutor(executor)
					switch path {
					case "select":
						selected, errSelect := manager.SelectAuth(
							context.Background(), "codex", routeModel, cliproxyexecutor.Options{},
						)
						if errSelect != nil {
							t.Fatal(errSelect)
						}
						if selected.ID != highID || !selected.Unavailable || !selected.Quota.Exceeded {
							t.Fatalf("selection must preserve the selected auth's actual state: %+v", selected)
						}
						if selected.Generation != high.Generation || len(executor.attempts) != 0 {
							t.Fatal("selection changed the credential generation or executed a request")
						}
						return
					case "execute":
						response, errExecute := manager.Execute(
							context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: routeModel},
							cliproxyexecutor.Options{},
						)
						if errExecute != nil {
							t.Fatal(errExecute)
						}
						if string(response.Payload) != lowID {
							t.Fatalf("response = %s, want healthy lower-priority account", response.Payload)
						}
					case "stream":
						result, errStream := manager.ExecuteStream(
							context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: routeModel},
							cliproxyexecutor.Options{Stream: true},
						)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil || string(chunk.Payload) != lowID {
								t.Fatalf("chunk = %+v, want healthy lower-priority account", chunk)
							}
						}
					}
					if len(executor.attempts) != 2 || executor.attempts[0] != highID || executor.attempts[1] != lowID {
						t.Fatalf("attempts = %v, want exhausted account then healthy account", executor.attempts)
					}
					for _, model := range executor.models {
						if model != targetModel {
							t.Fatalf("upstream model = %s, want %s", model, targetModel)
						}
					}
					results := hook.Results()
					if len(results) != 3 {
						t.Fatalf("results = %#v, want setup cooldown, exhausted account, then healthy account", results)
					}
					if results[0].AuthID != highID || results[0].Model != otherModel || results[0].Success {
						t.Fatalf("setup result = %#v, want the original other-model cooldown", results[0])
					}
					for index, id := range []string{highID, lowID} {
						current, _ := manager.GetByID(id)
						result := results[index+1]
						if result.AuthID != id || result.AuthGeneration != current.Generation ||
							result.Model != targetModel || result.Success != (index == 1) {
							t.Fatalf("result = %#v, want target and generation of %s", result, id)
						}
					}
					current, _ := manager.GetByID(highID)
					if current.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
						!current.Quota.Exceeded || time.Until(current.Quota.NextRecoverAt) < 59*time.Minute {
						t.Fatalf("credential quota did not retain its scope and original deadline: %#v", current)
					}
				},
			)
		}
	}
}

func TestOriginalManagerExecute_ModelAliasRequestNotBlockedByOtherModelQuotaCooldown(t *testing.T) {
	const (
		provider     = "antigravity"
		requestModel = "gemini-3.6-flash"
		targetModel  = "gemini-3.6-flash-high"
		imageModel   = "gemini-3.1-flash-image"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	executor := &originalAliasTargetExecutor{id: provider}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			provider: {
				{
					Name:  targetModel,
					Alias: requestModel,
					Fork:  true,
				},
			},
		},
	)

	now := time.Now()
	next := now.Add(1 * time.Hour)

	auth := &Auth{
		ID:       "antigravity-auth-1",
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			targetModel: {
				Status: StatusActive,
			},
			imageModel: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	updateAggregatedAvailability(auth, now)
	if !auth.Quota.Exceeded {
		t.Fatalf("precondition failed: auth.Quota.Exceeded should be true after updateAggregatedAvailability")
	}
	if auth.Unavailable {
		t.Fatalf("precondition failed: auth.Unavailable should be false since targetModel is active")
	}

	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, provider, []*registry.ModelInfo{
			{ID: requestModel},
			{ID: targetModel},
			{ID: imageModel},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)
	manager.RefreshSchedulerEntry(auth.ID)

	resp, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: requestModel},
		cliproxyexecutor.Options{},
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v, want success", errExecute)
	}
	if string(resp.Payload) != targetModel {
		t.Fatalf("Execute() payload = %q, want %q", string(resp.Payload), targetModel)
	}
}

func TestOriginalManagerSelectAuth_ModelAliasRequestNotBlockedByOtherModelQuotaCooldown(t *testing.T) {
	const (
		provider     = "antigravity"
		requestModel = "gemini-3.6-flash"
		targetModel  = "gemini-3.6-flash-high"
		imageModel   = "gemini-3.1-flash-image"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	executor := &originalAliasTargetExecutor{id: provider}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			provider: {
				{
					Name:  targetModel,
					Alias: requestModel,
					Fork:  true,
				},
			},
		},
	)

	now := time.Now()
	next := now.Add(1 * time.Hour)

	auth := &Auth{
		ID:       "antigravity-auth-2",
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			targetModel: {
				Status: StatusActive,
			},
			imageModel: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	updateAggregatedAvailability(auth, now)
	if !auth.Quota.Exceeded {
		t.Fatalf("precondition failed: auth.Quota.Exceeded should be true after updateAggregatedAvailability")
	}
	if auth.Unavailable {
		t.Fatalf("precondition failed: auth.Unavailable should be false since targetModel is active")
	}

	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, provider, []*registry.ModelInfo{
			{ID: requestModel},
			{ID: targetModel},
			{ID: imageModel},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)
	manager.RefreshSchedulerEntry(auth.ID)

	selected, errSelect := manager.SelectAuth(
		context.Background(),
		provider,
		requestModel,
		cliproxyexecutor.Options{},
	)
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v, want success", errSelect)
	}
	if selected == nil || selected.ID != auth.ID {
		t.Fatalf("SelectAuth() selected = %#v, want %s", selected, auth.ID)
	}
}

func TestOriginalManagerExecute_ModelAliasRequestBlockedWhenTargetModelInQuotaCooldown(t *testing.T) {
	const (
		provider     = "antigravity"
		requestModel = "gemini-3.6-flash"
		targetModel  = "gemini-3.6-flash-high"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	executor := &originalAliasTargetExecutor{id: provider}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(
		map[string][]internalconfig.OAuthModelAlias{
			provider: {
				{
					Name:  targetModel,
					Alias: requestModel,
					Fork:  true,
				},
			},
		},
	)

	now := time.Now()
	next := now.Add(1 * time.Hour)

	auth := &Auth{
		ID:       "antigravity-auth-3",
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			targetModel: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	updateAggregatedAvailability(auth, now)

	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		auth.ID, provider, []*registry.ModelInfo{
			{ID: requestModel},
			{ID: targetModel},
		},
	)
	t.Cleanup(
		func() {
			reg.UnregisterClient(auth.ID)
		},
	)
	manager.RefreshSchedulerEntry(auth.ID)

	_, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: requestModel},
		cliproxyexecutor.Options{},
	)
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want cooldown error")
	}
	var cooldownErr *modelCooldownError
	if !errors.As(errExecute, &cooldownErr) {
		t.Fatalf("Execute() error = %T (%v), want *modelCooldownError", errExecute, errExecute)
	}
	if cooldownErr.model != requestModel {
		t.Fatalf("cooldown model = %q, want %q", cooldownErr.model, requestModel)
	}
}

type originalAliasTargetExecutor struct {
	schedulerTestExecutor
	id, exhaustedID  string
	attempts, models []string
}

func (e *originalAliasTargetExecutor) Identifier() string { return e.id }

func (e *originalAliasTargetExecutor) Execute(
	_ context.Context, selected *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.attempts = append(e.attempts, selected.ID)
	e.models = append(e.models, req.Model)
	if selected.ID == e.exhaustedID {
		return cliproxyexecutor.Response{}, &routingSelectionCause{
			status: http.StatusTooManyRequests, body: []byte("account quota exhausted"), retryAfter: time.Hour,
			disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
				EffectScope:    cliproxyexecutor.StateEffectScopeCredential, EffectKey: selected.ID,
			},
		}
	}
	payload := req.Model
	if e.exhaustedID != "" {
		payload = selected.ID
	}
	return cliproxyexecutor.Response{Payload: []byte(payload)}, nil
}

func (e *originalAliasTargetExecutor) ExecuteStream(
	ctx context.Context, selected *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	response, err := e.Execute(ctx, selected, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
