package auth

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerRouteDecisionCapturesCandidatesAndSelection(t *testing.T) {
	const (
		provider = "route-decision-provider"
		model    = "route-decision-model"
	)
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(&routeTraceTestExecutor{provider: provider})
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{
			ID:       "route-decision-active",
			Provider: provider,
			Attributes: map[string]string{
				"priority":      "3",
				AttributeWeight: "5",
			},
		},
		&Auth{
			ID:       "route-decision-disabled",
			Provider: provider,
			Disabled: true,
			Attributes: map[string]string{
				"priority":      "9",
				AttributeWeight: "7",
			},
		},
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "route-decision-active" {
		t.Fatalf("response payload = %q", got)
	}

	event := findRouteTraceEvent(t, recorder.Events(), "route.decision", 1)
	if event.Provider != provider || event.Decision == nil {
		t.Fatalf("route decision event = %+v", event)
	}
	decision := event.Decision
	if decision.Strategy != "weighted-round-robin" || decision.RequestedModel != model ||
		decision.RoutedModel != model || len(decision.Providers) != 1 || decision.Providers[0] != provider {
		t.Fatalf("decision identity = %+v", decision)
	}
	if decision.CandidateScope != "complete" || len(decision.Candidates) != 2 ||
		decision.TruncatedCandidates != 0 || decision.Selected == nil {
		t.Fatalf("decision candidates/selection = %+v", decision)
	}
	if decision.Selected.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-decision-active") ||
		decision.Selected.MatchedRule != "weighted_round_robin" {
		t.Fatalf("decision selected = %+v", decision.Selected)
	}

	candidates := make(map[string]cliproxyexecutor.RouteDecisionCandidate, len(decision.Candidates))
	for _, candidate := range decision.Candidates {
		candidates[candidate.AuthIndex] = candidate
	}
	active := candidates[routeTraceTestAuthIndex(t, manager, "route-decision-active")]
	if !active.Selected || active.Priority != 3 || active.EffectiveWeight != 5 ||
		active.Native == nil || !*active.Native || active.Converted == nil || *active.Converted ||
		active.QuotaHeadroom != nil || active.InFlight != nil {
		t.Fatalf("active candidate = %+v", active)
	}
	if !containsRouteDecisionValue(active.MatchedRules, "highest_priority") ||
		!containsRouteDecisionValue(active.MatchedRules, "weighted_round_robin") {
		t.Fatalf("active matched rules = %v", active.MatchedRules)
	}
	disabled := candidates[routeTraceTestAuthIndex(t, manager, "route-decision-disabled")]
	if disabled.Priority != 9 || disabled.EffectiveWeight != 7 ||
		!containsRouteDecisionValue(disabled.ExcludedReasons, "disabled") ||
		!disabled.Cooldowns.Credential.Known || !disabled.Cooldowns.Credential.Active ||
		disabled.Cooldowns.Credential.Reason != "disabled" {
		t.Fatalf("disabled candidate = %+v", disabled)
	}
}

func TestRouteDecisionTruncationKeepsSelectedCandidate(t *testing.T) {
	opts := ensureRouteTraceState(routeTraceTestOptions(func(cliproxyexecutor.RouteTraceEvent) {}))
	draft := newRouteDecisionDraft(
		opts,
		[]string{"gemini", "claude"},
		"model",
		schedulerStrategyRoundRobin,
	)
	if draft == nil {
		t.Fatal("newRouteDecisionDraft() = nil")
	}
	var selected *Auth
	for index := range cliproxyexecutor.MaxRouteDecisionCandidates + 2 {
		auth := &Auth{ID: fmt.Sprintf("auth-%03d", index), Provider: "gemini"}
		draft.ensureAuth(auth, "model")
		if index == cliproxyexecutor.MaxRouteDecisionCandidates+1 {
			selected = auth
		}
	}
	draft.selectCandidate(selected, "model", "round_robin")

	if len(draft.snapshot.Candidates) != cliproxyexecutor.MaxRouteDecisionCandidates {
		t.Fatalf(
			"candidates = %d, want %d", len(draft.snapshot.Candidates), cliproxyexecutor.MaxRouteDecisionCandidates,
		)
	}
	if draft.snapshot.TruncatedCandidates != 2 {
		t.Fatalf("truncated candidates = %d, want 2", draft.snapshot.TruncatedCandidates)
	}
	if draft.snapshot.Selected == nil || draft.snapshot.Selected.AuthIndex != routeTraceAuthIndex(selected) {
		t.Fatalf("selected = %+v, want %s", draft.snapshot.Selected, routeTraceAuthIndex(selected))
	}
	if event := draft.event(opts); event.Provider != "mixed" {
		t.Fatalf("mixed event provider = %q", event.Provider)
	}
}

func TestManagerRouteDecisionCoversPluginAndCustomSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selector  Selector
		configure func(*Manager)
	}{
		{
			name:     "custom_selector",
			selector: &routeTraceMetadataSelector{},
		},
		{
			name:     "plugin_scheduler",
			selector: &RoundRobinSelector{},
			configure: func(manager *Manager) {
				manager.SetPluginScheduler(&routeTraceMetadataScheduler{})
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				provider := "route-decision-" + test.name
				model := provider + "-model"
				manager := NewManager(nil, test.selector, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(&routeTraceTestExecutor{provider: provider})
				if test.configure != nil {
					test.configure(manager)
				}
				registerRouteTraceTestAuths(
					t,
					manager,
					provider,
					model,
					&Auth{ID: provider + "-auth", Provider: provider},
				)
				recorder := &routeTraceTestRecorder{}

				if _, errExecute := manager.Execute(
					context.Background(),
					[]string{provider},
					cliproxyexecutor.Request{Model: model},
					routeTraceTestOptions(recorder.Add),
				); errExecute != nil {
					t.Fatalf("Execute() error = %v", errExecute)
				}
				decision := findRouteTraceEvent(t, recorder.Events(), "route.decision", 1).Decision
				if decision == nil || decision.Strategy != "custom" || decision.Selected == nil ||
					decision.Selected.MatchedRule != "custom" {
					t.Fatalf("decision = %+v", decision)
				}
				if len(decision.Candidates) != 1 ||
					containsRouteDecisionValue(decision.Candidates[0].MatchedRules, "highest_priority") {
					t.Fatalf("custom candidate rules = %+v", decision.Candidates)
				}
			},
		)
	}
}

func TestManagerRouteDecisionCapturesSessionAffinity(t *testing.T) {
	const (
		provider = "route-decision-affinity-provider"
		model    = "route-decision-affinity-model"
	)
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(&routeTraceTestExecutor{provider: provider})
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-decision-affinity-a", Provider: provider},
		&Auth{ID: "route-decision-affinity-b", Provider: provider},
	)
	headers := http.Header{"X-Session-ID": []string{"route-decision-session"}}
	if _, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Headers: headers},
	); errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}

	recorder := &routeTraceTestRecorder{}
	if _, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{
			Headers: headers,
			Metadata: map[string]any{
				cliproxyexecutor.RouteTraceCallbackMetadataKey: recorder.Add,
			},
		},
	); errExecute != nil {
		t.Fatalf("sticky Execute() error = %v", errExecute)
	}
	decision := findRouteTraceEvent(t, recorder.Events(), "route.decision", 1).Decision
	if decision == nil || decision.Strategy != "round-robin" ||
		decision.Affinity != "session_affinity" || decision.MatchedRule != "session_affinity" ||
		decision.Selected == nil || decision.Selected.MatchedRule != "session_affinity" {
		t.Fatalf("affinity decision = %+v", decision)
	}
}

func TestManagerRouteDecisionCapturesTransportAndGroupRules(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		model     string
		ctx       context.Context
		auths     []*Auth
		wantRules []string
	}{
		{
			name:     "websocket transport",
			provider: "codex",
			model:    "route-decision-websocket-model",
			ctx:      cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
			auths: []*Auth{
				{ID: "route-decision-http", Provider: "codex"},
				{
					ID:       "route-decision-websocket",
					Provider: "codex",
					Attributes: map[string]string{
						"websockets": "true",
					},
				},
			},
			wantRules: []string{"transport_preference"},
		},
		{
			name:     "virtual parent",
			provider: "gemini-cli",
			model:    "route-decision-virtual-parent-model",
			ctx:      context.Background(),
			auths: []*Auth{
				{
					ID:       "route-decision-parent-a::project",
					Provider: "gemini-cli",
					Attributes: map[string]string{
						"gemini_virtual_parent": "route-decision-parent-a",
					},
				},
				{
					ID:       "route-decision-parent-b::project",
					Provider: "gemini-cli",
					Attributes: map[string]string{
						"gemini_virtual_parent": "route-decision-parent-b",
					},
				},
			},
			wantRules: []string{"round_robin_group"},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(&routeTraceTestExecutor{provider: test.provider})
				registerRouteTraceTestAuths(t, manager, test.provider, test.model, test.auths...)
				recorder := &routeTraceTestRecorder{}

				if _, errExecute := manager.Execute(
					test.ctx,
					[]string{test.provider},
					cliproxyexecutor.Request{Model: test.model},
					routeTraceTestOptions(recorder.Add),
				); errExecute != nil {
					t.Fatalf("Execute() error = %v", errExecute)
				}
				decision := findRouteTraceEvent(t, recorder.Events(), "route.decision", 1).Decision
				if decision == nil || decision.Selected == nil {
					t.Fatalf("decision = %+v", decision)
				}
				var selected *cliproxyexecutor.RouteDecisionCandidate
				for index := range decision.Candidates {
					if decision.Candidates[index].Selected {
						selected = &decision.Candidates[index]
						break
					}
				}
				if selected == nil {
					t.Fatalf("selected candidate missing: %+v", decision)
				}
				for _, rule := range test.wantRules {
					if !containsRouteDecisionValue(selected.MatchedRules, rule) {
						t.Fatalf("selected matched rules = %v, want %q", selected.MatchedRules, rule)
					}
				}
			},
		)
	}
}

func TestMixedRoundRobinDoesNotReportVirtualParentTraversal(t *testing.T) {
	recorder := &routeTraceTestRecorder{}
	opts := ensureRouteTraceState(routeTraceTestOptions(recorder.Add))
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "a-project",
			Provider: "gemini-cli",
			Attributes: map[string]string{
				"gemini_virtual_parent": "parent-a",
			},
		},
		&Auth{
			ID:       "b-project",
			Provider: "gemini-cli",
			Attributes: map[string]string{
				"gemini_virtual_parent": "parent-b",
			},
		},
		&Auth{ID: "z-auth", Provider: "claude"},
	)
	picked, provider, errPick := scheduler.pickMixed(
		context.Background(), []string{"gemini-cli", "claude"}, "", opts, nil,
	)
	if errPick != nil || picked == nil || picked.ID != "a-project" || provider != "gemini-cli" {
		t.Fatalf("pickMixed() = (%#v, %q, %v), want a-project via gemini-cli", picked, provider, errPick)
	}
	decision := findRouteTraceEvent(t, recorder.Events(), "route.decision", 0).Decision
	if decision == nil || decision.Selected == nil {
		t.Fatalf("decision = %+v", decision)
	}
	for _, candidate := range decision.Candidates {
		if !candidate.Selected {
			continue
		}
		if containsRouteDecisionValue(candidate.MatchedRules, "round_robin_group") {
			t.Fatalf("mixed selected rules report unused grouped traversal: %v", candidate.MatchedRules)
		}
		return
	}
	t.Fatalf("selected candidate missing: %+v", decision.Candidates)
}

func TestRouteDecisionCooldownDoesNotReportExpiredStateAsActive(t *testing.T) {
	now := time.Now()
	cooldown := routeDecisionCooldown(
		true,
		now.Add(-time.Second),
		"quota_exceeded",
		"quota-family",
		9,
		StateResetAuthorityOperator,
		now,
	)
	if !cooldown.Known || cooldown.Active || cooldown.Indefinite || cooldown.RetryAfterMS != 0 ||
		cooldown.Reason != "" || cooldown.EffectKey != "quota-family" || cooldown.StateGeneration != 9 ||
		cooldown.ResetAuthority != string(StateResetAuthorityOperator) {
		t.Fatalf("expired cooldown = %+v", cooldown)
	}
}

func TestRouteDecisionCooldownsMatchSchedulerEligibilityWhenCoolingIsDisabled(t *testing.T) {
	const model = "route-decision-cooling-disabled-model"
	now := time.Now()
	auth := &Auth{
		ID:       "credential-secret.json",
		Provider: "gemini",
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    false,
				EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
				EffectKey:      model,
				Generation:     7,
				ResetAuthority: StateResetAuthoritySuccess,
			},
		},
	}
	cooldowns := routeDecisionCooldowns(auth, model, now)
	if cooldowns.ExactModel.Active || cooldowns.ExactModel.Indefinite ||
		cooldowns.ExactModel.EffectKey != model || cooldowns.ExactModel.StateGeneration != 7 {
		t.Fatalf("cooling-disabled exact-model state = %+v", cooldowns.ExactModel)
	}

	auth.ModelStates[model] = &ModelState{
		Status:         StatusError,
		Unavailable:    false,
		EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
		EffectKey:      "quota-family",
		Generation:     8,
		ResetAuthority: StateResetAuthoritySuccess,
		Quota: QuotaState{
			Exceeded:       true,
			Family:         "quota-family",
			Generation:     9,
			ResetAuthority: StateResetAuthoritySuccess,
		},
	}
	cooldowns = routeDecisionCooldowns(auth, model, now)
	if cooldowns.QuotaFamily.Active || cooldowns.QuotaFamily.Indefinite ||
		cooldowns.QuotaFamily.EffectKey != "quota-family" || cooldowns.QuotaFamily.StateGeneration != 9 {
		t.Fatalf("cooling-disabled quota-family state = %+v", cooldowns.QuotaFamily)
	}

	auth.ModelStates = nil
	auth.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
	auth.StateEffectKey = auth.ID
	auth.StateGeneration = 10
	auth.ResetAuthority = StateResetAuthoritySuccess
	cooldowns = routeDecisionCooldowns(auth, model, now)
	if cooldowns.Credential.Active || cooldowns.Credential.Indefinite ||
		cooldowns.Credential.EffectKey != routeTraceAuthIndex(auth) ||
		cooldowns.Credential.EffectKey == auth.ID || cooldowns.Credential.StateGeneration != 10 {
		t.Fatalf("cooling-disabled credential state = %+v", cooldowns.Credential)
	}
}

func TestRouteDecisionCallbackRunsAfterSchedulerUnlock(t *testing.T) {
	const (
		provider = "route-decision-reentrant-provider"
		model    = "route-decision-reentrant-model"
	)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(&routeTraceTestExecutor{provider: provider})
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-decision-reentrant-auth", Provider: provider},
	)
	var (
		once        sync.Once
		callbackErr error
		decision    *cliproxyexecutor.RouteDecisionSnapshot
	)
	callback := func(event cliproxyexecutor.RouteTraceEvent) {
		if event.Action != "route.decision" || event.Decision == nil {
			return
		}
		once.Do(
			func() {
				decision = event.Decision
				_, callbackErr = manager.Register(
					context.Background(),
					&Auth{ID: "route-decision-callback-added", Provider: provider, Status: StatusActive},
				)
			},
		)
	}
	done := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(
			context.Background(),
			[]string{provider},
			cliproxyexecutor.Request{Model: model},
			routeTraceTestOptions(callback),
		)
		done <- errExecute
	}()
	select {
	case errExecute := <-done:
		if errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("route decision callback blocked on scheduler lock")
	}
	if callbackErr != nil {
		t.Fatalf("callback Register() error = %v", callbackErr)
	}
	if decision == nil || len(decision.Candidates) != 1 {
		t.Fatalf("immutable decision candidates = %+v", decision)
	}
	if _, ok := manager.GetByID("route-decision-callback-added"); !ok {
		t.Fatal("callback-added auth is missing")
	}
}

func containsRouteDecisionValue(values []string, expected string) bool {
	return slices.Contains(values, expected)
}
