package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

const context1MTestModel = "context-1m-model"

type context1MCapturingSelector struct {
	selectedID  string
	selectedIDs []string
	candidates  []string
	mutate      func()
	calls       int
}

func (s *context1MCapturingSelector) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	s.candidates = s.candidates[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		s.candidates = append(s.candidates, auth.ID)
	}
	call := s.calls
	s.calls++
	if s.mutate != nil {
		s.mutate()
	}
	selectedID := s.selectedID
	if call < len(s.selectedIDs) {
		selectedID = s.selectedIDs[call]
	}
	for _, auth := range auths {
		if auth != nil && (selectedID == "" || auth.ID == selectedID) {
			return auth.Clone(), nil
		}
	}
	return nil, nil
}

type context1MPluginScheduler struct {
	authID     string
	authIDs    []string
	candidates []string
	mutate     func()
	calls      int
}

func (s *context1MPluginScheduler) PickAuth(
	_ context.Context,
	req pluginapi.SchedulerPickRequest,
) (pluginapi.SchedulerPickResponse, bool, error) {
	s.candidates = s.candidates[:0]
	for _, candidate := range req.Candidates {
		s.candidates = append(s.candidates, candidate.ID)
	}
	call := s.calls
	s.calls++
	if s.mutate != nil {
		s.mutate()
	}
	authID := s.authID
	if call < len(s.authIDs) {
		authID = s.authIDs[call]
	}
	return pluginapi.SchedulerPickResponse{AuthID: authID, Handled: true}, true, nil
}

type context1MTrackingExecutor struct {
	replaceAwareExecutor
	mu      sync.Mutex
	authIDs []string
}

func (e *context1MTrackingExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.recordAuth(auth)
	return cliproxyexecutor.Response{}, nil
}

func (e *context1MTrackingExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.recordAuth(auth)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *context1MTrackingExecutor) recordAuth(auth *Auth) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.mu.Unlock()
}

func (e *context1MTrackingExecutor) executedAuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

type context1MHomeDispatcher struct {
	payloads [][]byte
	calls    int
}

func (*context1MHomeDispatcher) HeartbeatOK() bool { return true }

func (d *context1MHomeDispatcher) RPopAuth(
	_ context.Context,
	_ string,
	_ string,
	_ http.Header,
	count int,
) ([]byte, error) {
	d.calls++
	if count <= 0 || count > len(d.payloads) {
		return nil, home.ErrAuthNotFound
	}
	return d.payloads[count-1], nil
}

func (*context1MHomeDispatcher) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	return nil, home.ErrAuthNotFound
}

func registerContext1MTestAuth(
	t *testing.T,
	manager *Manager,
	id string,
	provider string,
	contextLength int,
) *Auth {
	t.Helper()
	return registerContext1MTestAuthForModel(
		t, manager, id, provider, context1MTestModel, contextLength,
	)
}

func registerContext1MTestAuthForModel(
	t *testing.T,
	manager *Manager,
	id string,
	provider string,
	model string,
	contextLength int,
) *Auth {
	t.Helper()
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		id, provider, []*registry.ModelInfo{
			{
				ID:               model,
				CatalogName:      model,
				ContextLength:    contextLength,
				MaxContextLength: contextLength,
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
	auth := &Auth{ID: id, Provider: provider, Status: StatusActive}
	if manager != nil {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
	}
	return auth
}

func requireContext1MModelSupportError(t *testing.T, err error) {
	t.Helper()
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "model_not_supported" ||
		authErr.HTTPStatus != http.StatusBadRequest || authErr.Retryable {
		t.Fatalf("error = %#v, want non-retryable 400 model_not_supported", err)
	}
}

func TestPickNextSupportsDeepSeekFlashContext1MTag(t *testing.T) {
	const authID = "context-deepseek-flash"
	model := registry.LookupStaticModelInfo("deepseek-flash")
	if model == nil || model.ContextLength < context1MMinimumTokens {
		t.Fatalf("deepseek-flash metadata = %+v, want 1M context support", model)
	}
	if tagged := registry.LookupStaticModelInfo("deepseek-flash[1m]"); tagged != nil {
		t.Fatalf("static catalog contains request-only [1m] alias: %+v", tagged)
	}

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "claude"})
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{model})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	if _, err := manager.Register(
		context.Background(), &Auth{ID: authID, Provider: "claude", Status: StatusActive},
	); err != nil {
		t.Fatalf("Register(%s): %v", authID, err)
	}

	selected, selectedExecutor, err := manager.PickNext(
		context.Background(), "claude", "deepseek-flash[1m]", cliproxyexecutor.Options{}, nil,
	)
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if selected == nil || selected.ID != authID || selectedExecutor == nil {
		t.Fatalf("selected = %+v, executor = %+v", selected, selectedExecutor)
	}
}

func TestPickNextContext1MFiltersExactCredentialAcrossSelectors(t *testing.T) {
	t.Run(
		"built-in", func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
			registerContext1MTestAuth(t, manager, "context-low-built-in", "test", 200_000)
			registerContext1MTestAuth(t, manager, "context-high-built-in", "test", context1MMinimumTokens)

			selected, _, err := manager.PickNext(
				context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
			)
			if err != nil {
				t.Fatalf("PickNext: %v", err)
			}
			if selected == nil || selected.ID != "context-high-built-in" {
				t.Fatalf("selected = %+v, want high-context credential", selected)
			}
		},
	)

	t.Run(
		"custom", func(t *testing.T) {
			selector := &context1MCapturingSelector{selectedID: "context-high-custom"}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
			registerContext1MTestAuth(t, manager, "context-low-custom", "test", 200_000)
			registerContext1MTestAuth(t, manager, "context-high-custom", "test", context1MMinimumTokens)

			selected, _, err := manager.PickNext(
				context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
			)
			if err != nil {
				t.Fatalf("PickNext: %v", err)
			}
			if selected == nil || selected.ID != "context-high-custom" {
				t.Fatalf("selected = %+v, want high-context credential", selected)
			}
			if len(selector.candidates) != 1 || selector.candidates[0] != selected.ID {
				t.Fatalf("custom selector candidates = %v, want only high-context credential", selector.candidates)
			}
		},
	)

	t.Run(
		"plugin", func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
			registerContext1MTestAuth(t, manager, "context-low-plugin", "test", 200_000)
			registerContext1MTestAuth(t, manager, "context-high-plugin", "test", context1MMinimumTokens)
			scheduler := &context1MPluginScheduler{authID: "context-high-plugin"}
			manager.SetPluginScheduler(scheduler)

			selected, _, err := manager.PickNext(
				context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
			)
			if err != nil {
				t.Fatalf("PickNext: %v", err)
			}
			if selected == nil || selected.ID != "context-high-plugin" {
				t.Fatalf("selected = %+v, want high-context credential", selected)
			}
			if len(scheduler.candidates) != 1 || scheduler.candidates[0] != selected.ID {
				t.Fatalf("plugin candidates = %v, want only high-context credential", scheduler.candidates)
			}
		},
	)

	t.Run(
		"sticky", func(t *testing.T) {
			selector := NewSessionAffinitySelector(&RoundRobinSelector{})
			t.Cleanup(selector.Stop)
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
			low := registerContext1MTestAuth(t, manager, "context-low-sticky", "test", 200_000)
			registerContext1MTestAuth(t, manager, "context-high-sticky", "test", context1MMinimumTokens)
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"context-session"}}}
			selector.Remember("test", opts, low)

			selected, _, err := manager.PickNext(
				context.Background(), "test", context1MTestModel+"[1m]", opts, nil,
			)
			if err != nil {
				t.Fatalf("PickNext: %v", err)
			}
			if selected == nil || selected.ID != "context-high-sticky" {
				t.Fatalf("selected = %+v, want high-context credential", selected)
			}
		},
	)
}

func TestPickNextMixedContext1MUsesHighContextProvider(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "first"})
	manager.RegisterExecutor(&replaceAwareExecutor{id: "second"})
	registerContext1MTestAuth(t, manager, "context-low-mixed", "first", 200_000)
	registerContext1MTestAuth(t, manager, "context-high-mixed", "second", context1MMinimumTokens)

	selected, _, provider, err := manager.pickNextMixed(
		context.Background(),
		[]string{"first", "second"},
		context1MTestModel+"[1m]",
		cliproxyexecutor.Options{},
		nil,
	)
	if err != nil {
		t.Fatalf("pickNextMixed: %v", err)
	}
	if selected == nil || selected.ID != "context-high-mixed" || provider != "second" {
		t.Fatalf("selected = %+v provider = %q, want second high-context credential", selected, provider)
	}
}

func TestPickNextContext1MReturnsModelSupportErrorWhenAllCredentialsAreTooSmall(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
	registerContext1MTestAuth(t, manager, "context-low-one", "test", 200_000)
	registerContext1MTestAuth(t, manager, "context-low-two", "test", 500_000)

	selected, _, err := manager.PickNext(
		context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
	)
	if selected != nil {
		t.Fatalf("selected = %+v, want nil", selected)
	}
	requireContext1MModelSupportError(t, err)
}

func TestPickNextWithoutContext1MTagKeepsLowContextCredentialEligible(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
	registerContext1MTestAuth(t, manager, "context-low-ordinary", "test", 200_000)

	selected, _, err := manager.PickNext(
		context.Background(), "test", context1MTestModel, cliproxyexecutor.Options{}, nil,
	)
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if selected == nil || selected.ID != "context-low-ordinary" {
		t.Fatalf("selected = %+v, want ordinary low-context credential", selected)
	}
}

func TestCustomSelectorContext1MSelectionIsRevalidated(t *testing.T) {
	const (
		staleAuthID = "context-race-custom-stale"
		validAuthID = "context-race-custom-valid"
	)
	selector := &context1MCapturingSelector{selectedIDs: []string{staleAuthID, validAuthID}}
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
	registerContext1MTestAuth(t, manager, staleAuthID, "test", context1MMinimumTokens)
	registerContext1MTestAuth(t, manager, validAuthID, "test", context1MMinimumTokens)
	var mutateOnce sync.Once
	selector.mutate = func() {
		mutateOnce.Do(
			func() {
				registry.GetGlobalRegistry().RegisterClient(
					staleAuthID, "test", []*registry.ModelInfo{
						{
							ID: context1MTestModel, ContextLength: 200_000,
						},
					},
				)
			},
		)
	}

	selected, _, err := manager.PickNext(
		context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
	)
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if selected == nil || selected.ID != validAuthID {
		t.Fatalf("selected = %+v, want valid credential", selected)
	}
	if selector.calls != 2 {
		t.Fatalf("selector calls = %d, want 2", selector.calls)
	}
}

func TestPluginSchedulerContext1MSelectionIsRevalidated(t *testing.T) {
	const (
		staleAuthID = "context-race-plugin-stale"
		validAuthID = "context-race-plugin-valid"
	)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "test"})
	registerContext1MTestAuth(t, manager, staleAuthID, "test", context1MMinimumTokens)
	registerContext1MTestAuth(t, manager, validAuthID, "test", context1MMinimumTokens)
	scheduler := &context1MPluginScheduler{authIDs: []string{staleAuthID, validAuthID}}
	var mutateOnce sync.Once
	scheduler.mutate = func() {
		mutateOnce.Do(
			func() {
				registry.GetGlobalRegistry().RegisterClient(
					staleAuthID, "test", []*registry.ModelInfo{
						{
							ID: context1MTestModel, ContextLength: 200_000,
						},
					},
				)
			},
		)
	}
	manager.SetPluginScheduler(scheduler)

	selected, _, err := manager.PickNext(
		context.Background(), "test", context1MTestModel+"[1m]", cliproxyexecutor.Options{}, nil,
	)
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if selected == nil || selected.ID != validAuthID {
		t.Fatalf("selected = %+v, want valid credential", selected)
	}
	if scheduler.calls != 2 {
		t.Fatalf("scheduler calls = %d, want 2", scheduler.calls)
	}
}

func TestContext1MSchedulerSelectionErrorReclassifiesRegistryDowngrade(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := registerContext1MTestAuth(
		t, manager, "context-race-scheduler", "test", context1MMinimumTokens,
	)
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, "test", []*registry.ModelInfo{
			{
				ID: context1MTestModel, ContextLength: 200_000,
			},
		},
	)

	err := manager.context1MSchedulerSelectionError(
		[]*Auth{auth.Clone()},
		"test",
		context1MTestModel+"[1m]",
		&Error{Code: "auth_not_found", Message: "no auth available"},
	)
	requireContext1MModelSupportError(t, err)
}

func TestMixedSelectorsRetryAfterContext1MRegistryChange(t *testing.T) {
	const (
		staleAuthID = "context-race-mixed-stale"
		validAuthID = "context-race-mixed-valid"
	)
	t.Run(
		"custom", func(t *testing.T) {
			selector := &context1MCapturingSelector{selectedIDs: []string{staleAuthID, validAuthID}}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "first"})
			manager.RegisterExecutor(&replaceAwareExecutor{id: "second"})
			registerContext1MTestAuth(t, manager, staleAuthID, "first", context1MMinimumTokens)
			registerContext1MTestAuth(t, manager, validAuthID, "second", context1MMinimumTokens)
			var mutateOnce sync.Once
			selector.mutate = func() {
				mutateOnce.Do(
					func() {
						registry.GetGlobalRegistry().RegisterClient(
							staleAuthID, "first", []*registry.ModelInfo{
								{
									ID: context1MTestModel, ContextLength: 200_000,
								},
							},
						)
					},
				)
			}

			selected, _, provider, err := manager.pickNextMixed(
				context.Background(), []string{"first", "second"}, context1MTestModel+"[1m]",
				cliproxyexecutor.Options{}, nil,
			)
			if err != nil {
				t.Fatalf("pickNextMixed: %v", err)
			}
			if selected == nil || selected.ID != validAuthID || provider != "second" {
				t.Fatalf("selected = %+v provider = %q, want valid second credential", selected, provider)
			}
		},
	)

	t.Run(
		"plugin", func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(&replaceAwareExecutor{id: "first"})
			manager.RegisterExecutor(&replaceAwareExecutor{id: "second"})
			registerContext1MTestAuth(t, manager, staleAuthID, "first", context1MMinimumTokens)
			registerContext1MTestAuth(t, manager, validAuthID, "second", context1MMinimumTokens)
			scheduler := &context1MPluginScheduler{authIDs: []string{staleAuthID, validAuthID}}
			var mutateOnce sync.Once
			scheduler.mutate = func() {
				mutateOnce.Do(
					func() {
						registry.GetGlobalRegistry().RegisterClient(
							staleAuthID, "first", []*registry.ModelInfo{
								{
									ID: context1MTestModel, ContextLength: 200_000,
								},
							},
						)
					},
				)
			}
			manager.SetPluginScheduler(scheduler)

			selected, _, provider, err := manager.pickNextMixed(
				context.Background(), []string{"first", "second"}, context1MTestModel+"[1m]",
				cliproxyexecutor.Options{}, nil,
			)
			if err != nil {
				t.Fatalf("pickNextMixed: %v", err)
			}
			if selected == nil || selected.ID != validAuthID || provider != "second" {
				t.Fatalf("selected = %+v provider = %q, want valid second credential", selected, provider)
			}
		},
	)
}

func TestHomeContext1MSkipsLowContextCredentials(t *testing.T) {
	tests := []struct {
		name           string
		contextLengths []int
		wantAuthID     string
		wantError      bool
	}{
		{
			name: "uses next high context credential", contextLengths: []int{200_000, context1MMinimumTokens},
			wantAuthID: "home-context-2",
		},
		{name: "all credentials too small", contextLengths: []int{200_000, 500_000}, wantError: true},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				executor := &context1MTrackingExecutor{id: "test"}
				manager.RegisterExecutor(executor)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})

				dispatcher := &context1MHomeDispatcher{}
				for index, contextLength := range testCase.contextLengths {
					authID := fmt.Sprintf("home-context-%d", index+1)
					registerContext1MTestAuth(t, nil, authID, "test", contextLength)
					payload, err := json.Marshal(
						homeAuthDispatchResponse{
							AuthIndex: authID,
							Auth: Auth{
								ID:       authID,
								Provider: "test",
							},
						},
					)
					if err != nil {
						t.Fatalf("Marshal Home payload: %v", err)
					}
					dispatcher.payloads = append(dispatcher.payloads, payload)
				}
				manager.SetHomeClientProvider(func() HomeClient { return dispatcher })

				_, err := manager.Execute(
					context.Background(),
					[]string{"test"},
					cliproxyexecutor.Request{Model: context1MTestModel + "[1m]"},
					cliproxyexecutor.Options{},
				)
				if testCase.wantError {
					requireContext1MModelSupportError(t, err)
					if got := executor.executedAuthIDs(); len(got) != 0 {
						t.Fatalf("upstream executions = %v, want none", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if got := executor.executedAuthIDs(); len(got) != 1 || got[0] != testCase.wantAuthID {
					t.Fatalf("upstream executions = %v, want [%s]", got, testCase.wantAuthID)
				}
			},
		)
	}
}
