package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type originalCompactStatusError struct {
	code int
	msg  string
}

func (e originalCompactStatusError) Error() string   { return e.msg }
func (e originalCompactStatusError) StatusCode() int { return e.code }

type originalCompactExecutor struct {
	calls        int
	compactErr   error
	normalErr    error
	responseBody []byte
}

func (e *originalCompactExecutor) Identifier() string { return "compact-test-provider" }

func (e *originalCompactExecutor) Execute(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.calls++
	if opts.Alt == "responses/compact" {
		if e.compactErr != nil {
			return cliproxyexecutor.Response{}, e.compactErr
		}
	} else {
		if e.normalErr != nil {
			return cliproxyexecutor.Response{}, e.normalErr
		}
	}
	payload := e.responseBody
	if len(payload) == 0 {
		payload = []byte(`{"status":"ok"}`)
	}
	return cliproxyexecutor.Response{Payload: payload}, nil
}

func (e *originalCompactExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

func (e *originalCompactExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *originalCompactExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not supported")
}

func (e *originalCompactExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not supported")
}

// noinspection GoDirectComparisonOfErrors
func TestCompactOriginalUnknownOutcomeKeepsAvailability(t *testing.T) {
	executor := &originalCompactExecutor{
		compactErr: originalCompactStatusError{
			code: http.StatusInternalServerError,
			msg:  "upstream compact 500",
		},
	}
	m := NewManager(nil, nil, nil)
	m.SetHomeClientProvider(func() HomeClient { return nil })
	m.RegisterExecutor(executor)

	model := "gpt-5.6-sol"
	auth1 := &Auth{ID: "auth1", Provider: executor.Identifier(), Status: StatusActive}
	auth2 := &Auth{ID: "auth2", Provider: executor.Identifier(), Status: StatusActive}
	if _, err := m.Register(context.Background(), auth1); err != nil {
		t.Fatalf("Register auth1: %v", err)
	}
	if _, err := m.Register(context.Background(), auth2); err != nil {
		t.Fatalf("Register auth2: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth1.ID, auth1.Provider, []*registry.ModelInfo{{ID: model}})
	registry.GetGlobalRegistry().RegisterClient(auth2.ID, auth2.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth1.ID)
			registry.GetGlobalRegistry().UnregisterClient(auth2.ID)
		},
	)

	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}

	start := time.Now()
	_, errExec := m.Execute(context.Background(), []string{executor.Identifier()}, req, opts)
	elapsed := time.Since(start)

	if errExec == nil {
		t.Fatal("Execute expected error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Execute took %v, should not pause for cooldown wait", elapsed)
	}
	if executor.calls != 1 {
		t.Fatalf("executor.calls = %d, want 1 for an unknown processing outcome", executor.calls)
	}
	if errExec != executor.compactErr {
		t.Fatalf("Execute error = %v, want the original compact failure", errExec)
	}

	// Verify model states are not unavailable
	for _, id := range []string{"auth1", "auth2"} {
		a, ok := m.GetByID(id)
		if !ok {
			t.Fatalf("auth %s not found", id)
		}
		if state, exists := a.ModelStates[model]; exists && state != nil {
			if state.Unavailable {
				t.Fatalf("auth %s marked unavailable after compact failure", id)
			}
			if !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("auth %s has NextRetryAfter set in future: %v", id, state.NextRetryAfter)
			}
		}
	}

	// Normal request succeeds immediately
	normalReq := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	normalOpts := cliproxyexecutor.Options{}
	resp, errNormal := m.Execute(context.Background(), []string{executor.Identifier()}, normalReq, normalOpts)
	if errNormal != nil {
		t.Fatalf("normal Execute failed: %v", errNormal)
	}
	if string(resp.Payload) != `{"status":"ok"}` {
		t.Fatalf("normal Execute payload = %s", string(resp.Payload))
	}
}

func TestOriginalManager_ResponsesCompact_RequestFault_StopsFallback(t *testing.T) {
	executor := &originalCompactExecutor{
		compactErr: originalCompactStatusError{code: http.StatusNotFound, msg: "404 endpoint not found"},
	}
	m := NewManager(nil, nil, nil)
	m.SetHomeClientProvider(func() HomeClient { return nil })
	m.RegisterExecutor(executor)

	model := "gpt-5.6-sol"
	auth1 := &Auth{ID: "auth1", Provider: executor.Identifier(), Status: StatusActive}
	auth2 := &Auth{ID: "auth2", Provider: executor.Identifier(), Status: StatusActive}
	if _, err := m.Register(context.Background(), auth1); err != nil {
		t.Fatalf("Register auth1: %v", err)
	}
	if _, err := m.Register(context.Background(), auth2); err != nil {
		t.Fatalf("Register auth2: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth1.ID, auth1.Provider, []*registry.ModelInfo{{ID: model}})
	registry.GetGlobalRegistry().RegisterClient(auth2.ID, auth2.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth1.ID)
			registry.GetGlobalRegistry().UnregisterClient(auth2.ID)
		},
	)

	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}

	_, errExec := m.Execute(context.Background(), []string{executor.Identifier()}, req, opts)
	if errExec == nil {
		t.Fatal("Execute expected error, got nil")
	}
	if executor.calls != 1 {
		t.Fatalf("executor.calls = %d, want 1 (fallback stopped on request fault)", executor.calls)
	}

	// Verify model states are not unavailable
	for _, id := range []string{"auth1", "auth2"} {
		a, ok := m.GetByID(id)
		if !ok {
			t.Fatalf("auth %s not found", id)
		}
		if state, exists := a.ModelStates[model]; exists && state != nil {
			if state.Unavailable {
				t.Fatalf("auth %s marked unavailable after compact 404 fault", id)
			}
		}
	}
}

func TestOriginalManager_ResponsesCompact_Unauthorized_CoolsCredential(t *testing.T) {
	executor := &originalCompactExecutor{
		compactErr: originalCompactStatusError{code: http.StatusUnauthorized, msg: "401 unauthorized"},
	}
	m := NewManager(nil, nil, nil)
	m.SetHomeClientProvider(func() HomeClient { return nil })
	m.RegisterExecutor(executor)

	model := "gpt-5.6-sol"
	auth1 := &Auth{ID: "auth1", Provider: executor.Identifier(), Status: StatusActive}
	if _, err := m.Register(context.Background(), auth1); err != nil {
		t.Fatalf("Register auth1: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth1.ID, auth1.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth1.ID)
		},
	)

	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}

	_, errExec := m.Execute(context.Background(), []string{executor.Identifier()}, req, opts)
	if errExec == nil {
		t.Fatal("Execute expected error, got nil")
	}

	a, ok := m.GetByID("auth1")
	if !ok {
		t.Fatal("auth1 not found")
	}
	if !a.Unavailable || a.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
		a.StateGeneration == 0 {
		t.Fatal("auth1 credential should be unavailable after 401 unauthorized")
	}
	if a.NextRetryAfter.IsZero() || !a.NextRetryAfter.After(time.Now()) {
		t.Fatalf("auth1 NextRetryAfter not set in future for 401 unauthorized: %v", a.NextRetryAfter)
	}
	if state := a.ModelStates[model]; state != nil && state.Unavailable {
		t.Fatal("credential cooldown was copied into an independent model state")
	}
	for _, requested := range []string{model, "sibling-model"} {
		if blocked, _, _ := isAuthBlockedForModel(a, requested, time.Now()); !blocked {
			t.Fatalf("credential unauthorized state did not block %s", requested)
		}
	}
}

func TestOriginalManager_ResponsesCompact_Forbidden_CoolsCredential(t *testing.T) {
	executor := &originalCompactExecutor{
		compactErr: originalCompactStatusError{code: http.StatusForbidden, msg: "403 forbidden"},
	}
	m := NewManager(nil, nil, nil)
	m.SetHomeClientProvider(func() HomeClient { return nil })
	m.RegisterExecutor(executor)

	model := "gpt-5.6-sol"
	auth1 := &Auth{ID: "auth1", Provider: executor.Identifier(), Status: StatusActive}
	if _, err := m.Register(context.Background(), auth1); err != nil {
		t.Fatalf("Register auth1: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth1.ID, auth1.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth1.ID)
		},
	)

	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}

	_, errExec := m.Execute(context.Background(), []string{executor.Identifier()}, req, opts)
	if errExec == nil {
		t.Fatal("Execute expected error, got nil")
	}

	a, ok := m.GetByID("auth1")
	if !ok {
		t.Fatal("auth1 not found")
	}
	if !a.Unavailable || a.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
		a.StateGeneration == 0 {
		t.Fatal("auth1 credential should be unavailable after 403 forbidden")
	}
	if a.NextRetryAfter.IsZero() || !a.NextRetryAfter.After(time.Now()) {
		t.Fatalf("auth1 NextRetryAfter not set in future for 403 forbidden: %v", a.NextRetryAfter)
	}
	if state := a.ModelStates[model]; state != nil && state.Unavailable {
		t.Fatal("credential cooldown was copied into an independent model state")
	}
	for _, requested := range []string{model, "sibling-model"} {
		if blocked, _, _ := isAuthBlockedForModel(a, requested, time.Now()); !blocked {
			t.Fatalf("credential forbidden state did not block %s", requested)
		}
	}
}

func TestOriginalManager_ResponsesCompact_Quota429_CoolsCredential(t *testing.T) {
	executor := &originalCompactExecutor{
		compactErr: originalCompactStatusError{
			code: http.StatusTooManyRequests, msg: `{"error":{"type":"usage_limit_reached","message":"quota ` +
				`exceeded"}}`,
		},
	}
	m := NewManager(nil, nil, nil)
	m.SetHomeClientProvider(func() HomeClient { return nil })
	m.RegisterExecutor(executor)

	model := "gpt-5.6-sol"
	auth1 := &Auth{ID: "auth1", Provider: executor.Identifier(), Status: StatusActive}
	if _, err := m.Register(context.Background(), auth1); err != nil {
		t.Fatalf("Register auth1: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth1.ID, auth1.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth1.ID)
		},
	)

	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}

	_, errExec := m.Execute(context.Background(), []string{executor.Identifier()}, req, opts)
	if errExec == nil {
		t.Fatal("Execute expected error, got nil")
	}

	a, ok := m.GetByID("auth1")
	if !ok {
		t.Fatal("auth1 not found")
	}
	state, exists := a.ModelStates[model]
	if !exists || state == nil {
		t.Fatal("auth1 model state should be recorded for 429 quota")
	}
	if !state.Quota.Exceeded {
		t.Fatal("auth1 quota should be marked exceeded after 429 quota")
	}
	if state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily || state.Generation == 0 ||
		state.ResetAuthority == "" {
		t.Fatal("quota cooldown lost its typed scope, generation or reset authority")
	}
	if state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(time.Now()) {
		t.Fatalf("auth1 NextRetryAfter not set in future for 429 quota: %v", state.NextRetryAfter)
	}
}
